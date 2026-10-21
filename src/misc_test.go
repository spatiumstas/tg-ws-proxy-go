package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"testing"
	"time"
)

func TestGenerateRelayInit(t *testing.T) {
	ri := generateRelayInit(protoTagAbridged, 2)
	if len(ri) != handshakeLen {
		t.Fatalf("relayInit len = %d, want %d", len(ri), handshakeLen)
	}
	if reservedFirst[ri[0]] {
		t.Error("first byte must not be a reserved value")
	}
	for _, prefix := range reservedStart {
		if bytes.Equal(ri[:4], prefix) {
			t.Errorf("reserved relay prefix: %x", prefix)
		}
	}
	if bytes.Equal(ri[4:8], []byte{0, 0, 0, 0}) {
		t.Error("bytes [4:8] must not be all-zero")
	}
	if bytes.Equal(ri, generateRelayInit(protoTagAbridged, 2)) {
		t.Error("relayInit must be randomized per call")
	}
}

func TestFakeTLSConnectLink(t *testing.T) {
	got := fakeTLSConnectLink("1.2.3.4", 443, "00112233445566778899aabbccddeeff", "example.com")
	want := "tg://proxy?server=1.2.3.4&port=443&secret=ee00112233445566778899aabbccddeeff6578616d706c652e636f6d"
	if got != want {
		t.Errorf("link = %q\nwant   %q", got, want)
	}
}

func TestParseIPCSV(t *testing.T) {
	got, err := parseIPCSV("1.2.3.4, 5.6.7.8 , 1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "1.2.3.4" || got[1] != "5.6.7.8" {
		t.Errorf("parseIPCSV = %v, want dedup [1.2.3.4 5.6.7.8]", got)
	}
	if _, err := parseIPCSV("not-an-ip"); err == nil {
		t.Error("expected error for invalid IP")
	}
	if _, err := parseIPCSV("  "); err == nil {
		t.Error("expected error for empty pool")
	}
}

func TestSplitterFlush(t *testing.T) {
	ms := &msgSplitter{proto: protoIntermediateInt}
	plain := buildIntermediate(100) // 104-byte packet
	ct := plain

	if parts := ms.split(ct[:10]); len(parts) != 0 {
		t.Fatalf("incomplete packet should yield 0 parts, got %d", len(parts))
	}
	flushed := ms.flush()
	if len(flushed) != 1 || !bytes.Equal(flushed[0], ct[:10]) {
		t.Fatal("flush must return the buffered partial tail")
	}
	if got := ms.flush(); got != nil {
		t.Error("second flush should be empty")
	}
}

func TestVerifyFakeTLSStaleTimestamp(t *testing.T) {
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)

	data := make([]byte, 76)
	data[0] = tlsRecordHandshake
	data[5] = 0x01
	data[43] = 0x20
	_, _ = rand.Read(data[tlsSessionIDOffset : tlsSessionIDOffset+tlsSessionIDLen])

	zeroed := make([]byte, len(data))
	copy(zeroed, data)
	for i := 0; i < tlsClientRandomLen; i++ {
		zeroed[tlsClientRandomOffset+i] = 0
	}
	expected := hmacSHA256(secret, zeroed)
	copy(data[tlsClientRandomOffset:tlsClientRandomOffset+28], expected[:28])

	// timestamp far outside tolerance -> must be rejected even with valid HMAC
	var tb [4]byte
	binary.LittleEndian.PutUint32(tb[:], uint32(time.Now().Unix()-1000))
	for i := 0; i < 4; i++ {
		data[tlsClientRandomOffset+28+i] = tb[i] ^ expected[28+i]
	}
	if _, _, ok := verifyFakeTLSClientHello(data, secret); ok {
		t.Error("stale timestamp must be rejected")
	}
}
