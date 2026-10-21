package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"testing"
)

func TestReverseBytes(t *testing.T) {
	in := []byte{1, 2, 3, 4}
	got := reverseBytes(in)
	if !bytes.Equal(got, []byte{4, 3, 2, 1}) {
		t.Errorf("reverseBytes = %v", got)
	}
	if !bytes.Equal(in, []byte{1, 2, 3, 4}) {
		t.Error("reverseBytes mutated input")
	}
}

func TestProtoFromTag(t *testing.T) {
	if protoFromTag(protoTagAbridged) != protoAbridgedInt {
		t.Error("abridged tag")
	}
	if protoFromTag(protoTagIntermediate) != protoIntermediateInt {
		t.Error("intermediate tag")
	}
	if protoFromTag(protoTagSecure) != protoPaddedIntermediateInt {
		t.Error("secure tag")
	}
}

func TestSignedDC(t *testing.T) {
	if signedDC(2, false) != 2 {
		t.Error("non-media should be positive")
	}
	if signedDC(2, true) != -2 {
		t.Error("media should be negative")
	}
}

func TestWSDomains(t *testing.T) {
	main := wsDomains(1, false)
	if len(main) != 1 || main[0] != "kws1.web.telegram.org" {
		t.Errorf("wsDomains(1,false) = %v", main)
	}
	media := wsDomains(5, true)
	if media[0] != "kws5-1.web.telegram.org" || media[1] != "kws5.web.telegram.org" {
		t.Errorf("wsDomains(5,true) = %v", media)
	}
}

func TestFallbackIP(t *testing.T) {
	if fallbackIP(1) != "149.154.175.50" {
		t.Errorf("fallbackIP(1) = %q", fallbackIP(1))
	}
	if fallbackIP(999) != "" {
		t.Errorf("fallbackIP(999) should be empty, got %q", fallbackIP(999))
	}
}

func TestKeyFromPrekeyAndSecretDeterministic(t *testing.T) {
	prekey := bytes.Repeat([]byte{0xAB}, 32)
	secret := []byte("0123456789abcdef")
	a := keyFromPrekeyAndSecret(prekey, secret)
	b := keyFromPrekeyAndSecret(prekey, secret)
	if a != b {
		t.Error("key derivation must be deterministic")
	}
	c := keyFromPrekeyAndSecret(bytes.Repeat([]byte{0xAC}, 32), secret)
	if a == c {
		t.Error("different prekey must yield different key")
	}
}

// craftHandshake builds a 64-byte client handshake that decrypts (under secret)
// to the given proto tag and signed DC index, mirroring how a real client frames it.
func craftHandshake(t *testing.T, secret, protoTag []byte, dcIdx int16) []byte {
	t.Helper()
	hs := make([]byte, handshakeLen)
	if _, err := rand.Read(hs[:protoTagPos]); err != nil {
		t.Fatal(err)
	}
	prekey := hs[skipLen : skipLen+prekeyLen]
	iv := hs[skipLen+prekeyLen : skipLen+prekeyLen+ivLen]
	key := keyFromPrekeyAndSecret(prekey, secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	// keystream over the 64 bytes (tail currently zero -> out tail == keystream)
	ks := cipher.NewCTR(block, iv)
	out := make([]byte, handshakeLen)
	ks.XORKeyStream(out, hs)

	desired := make([]byte, 8)
	copy(desired[:4], protoTag)
	binary.LittleEndian.PutUint16(desired[4:6], uint16(dcIdx))
	for i := 0; i < 8; i++ {
		hs[protoTagPos+i] = desired[i] ^ out[protoTagPos+i]
	}
	return hs
}

func TestTryHandshakeRoundTrip(t *testing.T) {
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}

	hs := craftHandshake(t, secret, protoTagIntermediate, 2)
	hi, ok := tryHandshake(hs, secret)
	if !ok {
		t.Fatal("expected valid handshake")
	}
	if hi.DC != 2 || hi.IsMedia {
		t.Errorf("DC=%d media=%v, want DC=2 non-media", hi.DC, hi.IsMedia)
	}
	if !bytes.Equal(hi.ProtoTag, protoTagIntermediate) {
		t.Errorf("proto tag = %v", hi.ProtoTag)
	}

	// media (negative DC index)
	hsm := craftHandshake(t, secret, protoTagAbridged, -4)
	him, ok := tryHandshake(hsm, secret)
	if !ok || him.DC != 4 || !him.IsMedia {
		t.Errorf("media handshake: ok=%v DC=%d media=%v", ok, him.DC, him.IsMedia)
	}

	// invalid proto tag -> rejected
	bad := craftHandshake(t, secret, []byte{0x01, 0x02, 0x03, 0x04}, 2)
	if _, ok := tryHandshake(bad, secret); ok {
		t.Error("expected invalid proto tag to be rejected")
	}

	// wrong length -> rejected
	if _, ok := tryHandshake(make([]byte, 10), secret); ok {
		t.Error("short handshake must be rejected")
	}
}

func TestBuildCiphersTranscodeRoundTrip(t *testing.T) {
	secret := make([]byte, 16)
	clientDecI := make([]byte, prekeyLen+ivLen)
	relayInit := make([]byte, handshakeLen)
	_, _ = rand.Read(secret)
	_, _ = rand.Read(clientDecI)
	_, _ = rand.Read(relayInit)

	// Proxy ciphers and an identical "peer" set (same inputs -> same streams).
	cltDec, _, tgEnc, _, err := buildCiphers(clientDecI, relayInit, secret)
	if err != nil {
		t.Fatal(err)
	}
	peerCltDec, _, peerTgEnc, _, err := buildCiphers(clientDecI, relayInit, secret)
	if err != nil {
		t.Fatal(err)
	}

	plain := []byte("the quick brown fox jumps over the lazy dog")
	// client encrypts with its send stream (== proxy cltDec)
	enc := make([]byte, len(plain))
	peerCltDec.XORKeyStream(enc, plain)
	// proxy decrypts then re-encrypts for Telegram
	cltDec.XORKeyStream(enc, enc)
	tgEnc.XORKeyStream(enc, enc)
	// Telegram decrypts with its recv stream (== proxy tgEnc)
	peerTgEnc.XORKeyStream(enc, enc)
	if !bytes.Equal(enc, plain) {
		t.Errorf("transcode round-trip mismatch: got %q want %q", enc, plain)
	}
}
