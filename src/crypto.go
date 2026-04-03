package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strconv"
)

func tryHandshake(handshake, secret []byte) (*handshakeInfo, bool) {
	if len(handshake) != handshakeLen {
		return nil, false
	}
	decPrekeyAndIV := handshake[skipLen : skipLen+prekeyLen+ivLen]
	decPrekey := decPrekeyAndIV[:prekeyLen]
	decIV := decPrekeyAndIV[prekeyLen:]

	h := keyFromPrekeyAndSecret(decPrekey, secret)
	block, err := aes.NewCipher(h[:])
	if err != nil {
		return nil, false
	}
	dec := cipher.NewCTR(block, decIV)
	decrypted := make([]byte, len(handshake))
	dec.XORKeyStream(decrypted, handshake)

	protoTag := decrypted[protoTagPos : protoTagPos+4]
	if !bytes.Equal(protoTag, protoTagAbridged) && !bytes.Equal(protoTag, protoTagIntermediate) && !bytes.Equal(protoTag, protoTagSecure) {
		return nil, false
	}

	dcIdx := int16(binary.LittleEndian.Uint16(decrypted[dcIdxPos : dcIdxPos+2]))
	dc := int(math.Abs(float64(dcIdx)))
	isMedia := dcIdx < 0

	pt := make([]byte, 4)
	copy(pt, protoTag)
	civ := make([]byte, len(decPrekeyAndIV))
	copy(civ, decPrekeyAndIV)
	return &handshakeInfo{DC: dc, IsMedia: isMedia, ProtoTag: pt, ClientDecI: civ}, true
}

func generateRelayInit(protoTag []byte, dcIdx int16) []byte {
	rnd := make([]byte, handshakeLen)
	for {
		_, _ = rand.Read(rnd)
		if reservedFirst[rnd[0]] {
			continue
		}
		bad := false
		for _, rs := range reservedStart {
			if bytes.Equal(rnd[:4], rs) {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		if bytes.Equal(rnd[4:8], []byte{0, 0, 0, 0}) {
			continue
		}
		break
	}

	encKey := rnd[skipLen : skipLen+prekeyLen]
	encIV := rnd[skipLen+prekeyLen : skipLen+prekeyLen+ivLen]
	block, _ := aes.NewCipher(encKey)
	enc := cipher.NewCTR(block, encIV)
	encryptedFull := make([]byte, handshakeLen)
	enc.XORKeyStream(encryptedFull, rnd)

	tailPlain := make([]byte, 8)
	copy(tailPlain[:4], protoTag)
	binary.LittleEndian.PutUint16(tailPlain[4:6], uint16(dcIdx))
	_, _ = rand.Read(tailPlain[6:8])

	result := make([]byte, handshakeLen)
	copy(result, rnd)
	for i := 0; i < 8; i++ {
		keystream := encryptedFull[56+i] ^ rnd[56+i]
		result[56+i] = tailPlain[i] ^ keystream
	}
	return result
}

func buildCiphers(clientDecPrekeyAndIV, relayInit, secret []byte) (cltDec, cltEnc, tgEnc, tgDec cipher.Stream, err error) {
	cltDecPrekey := clientDecPrekeyAndIV[:prekeyLen]
	cltDecIV := clientDecPrekeyAndIV[prekeyLen:]
	k1 := keyFromPrekeyAndSecret(cltDecPrekey, secret)
	b1, err := aes.NewCipher(k1[:])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cltDec = cipher.NewCTR(b1, cltDecIV)

	rev := reverseBytes(clientDecPrekeyAndIV)
	encPrekey := rev[:prekeyLen]
	encIV := rev[prekeyLen:]
	k2 := keyFromPrekeyAndSecret(encPrekey, secret)
	b2, err := aes.NewCipher(k2[:])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cltEnc = cipher.NewCTR(b2, encIV)

	relayEncKey := relayInit[8:40]
	relayEncIV := relayInit[40:56]
	b3, err := aes.NewCipher(relayEncKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tgEnc = cipher.NewCTR(b3, relayEncIV)

	relayDecPI := reverseBytes(relayInit[8:56])
	relayDecKey := relayDecPI[:keyLen]
	relayDecIV := relayDecPI[keyLen:]
	b4, err := aes.NewCipher(relayDecKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tgDec = cipher.NewCTR(b4, relayDecIV)

	zeros := make([]byte, handshakeLen)
	tmp := make([]byte, handshakeLen)
	cltDec.XORKeyStream(tmp, zeros)
	tgEnc.XORKeyStream(tmp, zeros)

	return cltDec, cltEnc, tgEnc, tgDec, nil
}

func keyFromPrekeyAndSecret(prekey, secret []byte) [32]byte {
	b := make([]byte, 0, len(prekey)+len(secret))
	b = append(b, prekey...)
	b = append(b, secret...)
	return sha256.Sum256(b)
}

func reverseBytes(in []byte) []byte {
	out := make([]byte, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

func protoFromTag(tag []byte) uint32 {
	switch {
	case bytes.Equal(tag, protoTagAbridged):
		return protoAbridgedInt
	case bytes.Equal(tag, protoTagIntermediate):
		return protoIntermediateInt
	default:
		return protoPaddedIntermediateInt
	}
}

func wsDomains(dc int, isMedia bool) []string {
	dcS := strconv.Itoa(dc)
	if isMedia {
		return []string{
			"kws" + dcS + "-1.web.telegram.org",
			"kws" + dcS + ".web.telegram.org",
		}
	}
	return []string{
		"kws" + dcS + ".web.telegram.org",
		"kws" + dcS + "-1.web.telegram.org",
	}
}

func signedDC(dc int, media bool) int16 {
	if media {
		return int16(-dc)
	}
	return int16(dc)
}

func fallbackIP(dc int) string {
	if ip, ok := dcFallbackDefaults[dc]; ok {
		return ip
	}
	return ""
}
