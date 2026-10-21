package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestFakeTLSFramingRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	wc := &fakeTLSConn{raw: a}
	rc := &fakeTLSConn{raw: b}

	payload := make([]byte, 40000) // exceeds one TLS record (16384) -> multiple records
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	go func() {
		_, _ = wc.Write(payload)
		_ = a.Close()
	}()

	got := make([]byte, 0, len(payload))
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("framing round-trip mismatch: got %d bytes want %d", len(got), len(payload))
	}
}

func TestVerifyFakeTLSClientHello(t *testing.T) {
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 76)
	data[0] = tlsRecordHandshake
	data[5] = 0x01  // ClientHello
	data[43] = 0x20 // session id length = 32
	if _, err := rand.Read(data[tlsSessionIDOffset : tlsSessionIDOffset+tlsSessionIDLen]); err != nil {
		t.Fatal(err)
	}

	// HMAC is computed over the record with the client-random region zeroed.
	zeroed := make([]byte, len(data))
	copy(zeroed, data)
	for i := 0; i < tlsClientRandomLen; i++ {
		zeroed[tlsClientRandomOffset+i] = 0
	}
	expected := hmacSHA256(secret, zeroed)

	copy(data[tlsClientRandomOffset:tlsClientRandomOffset+28], expected[:28])
	var tb [4]byte
	binary.LittleEndian.PutUint32(tb[:], uint32(time.Now().Unix()))
	for i := 0; i < 4; i++ {
		data[tlsClientRandomOffset+28+i] = tb[i] ^ expected[28+i]
	}

	cr, sid, ok := verifyFakeTLSClientHello(data, secret)
	if !ok {
		t.Fatal("expected valid fake-TLS ClientHello")
	}
	if !bytes.Equal(cr, data[tlsClientRandomOffset:tlsClientRandomOffset+tlsClientRandomLen]) {
		t.Error("client random mismatch")
	}
	if !bytes.Equal(sid, data[tlsSessionIDOffset:tlsSessionIDOffset+tlsSessionIDLen]) {
		t.Error("session id mismatch")
	}

	// Tampering the HMAC region must fail verification.
	data[tlsClientRandomOffset] ^= 0xFF
	if _, _, ok := verifyFakeTLSClientHello(data, secret); ok {
		t.Error("tampered ClientHello must be rejected")
	}
}

func TestFakeTLSServerHelloStructure(t *testing.T) {
	sessionID := bytes.Repeat([]byte{0x55}, 32)
	out, err := buildFakeTLSServerHello(make([]byte, 16), make([]byte, 32), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	next := 5 + int(binary.BigEndian.Uint16(out[3:5]))
	want := []byte{0x14, 0x03, 0x03, 0x00, 0x01, 0x01, 0x17, 0x03, 0x03}
	if next+len(want) > len(out) || !bytes.Equal(out[next:next+len(want)], want) {
		t.Fatalf("invalid CCS and application data headers at byte %d", next)
	}
	if out[43] != 32 || !bytes.Equal(out[44:76], sessionID) {
		t.Error("server hello must echo the 32-byte session ID")
	}
}

func TestWrapFakeTLSRecordsStructure(t *testing.T) {
	out := wrapFakeTLSRecords(bytes.Repeat([]byte{0xAA}, 5))
	if len(out) != 5+5 {
		t.Fatalf("wrapped len = %d, want 10 (5 header + 5 payload)", len(out))
	}
	if out[0] != tlsRecordAppData || out[1] != 0x03 || out[2] != 0x03 {
		t.Error("bad record header")
	}
	if int(binary.BigEndian.Uint16(out[3:5])) != 5 {
		t.Error("bad record length field")
	}
	if wrapFakeTLSRecords(nil) != nil {
		t.Error("empty input should produce nil")
	}
}
