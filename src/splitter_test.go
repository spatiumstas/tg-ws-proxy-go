package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"testing"
)

func testRelayInit(t *testing.T) []byte {
	t.Helper()
	ri := make([]byte, handshakeLen)
	if _, err := rand.Read(ri); err != nil {
		t.Fatal(err)
	}
	return ri
}

func encForSplitter(t *testing.T, relayInit, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(relayInit[8:40])
	if err != nil {
		t.Fatal(err)
	}
	enc := cipher.NewCTR(block, relayInit[40:56])
	tmp := make([]byte, handshakeLen)
	enc.XORKeyStream(tmp, make([]byte, handshakeLen))
	out := make([]byte, len(plain))
	enc.XORKeyStream(out, plain)
	return out
}

func buildIntermediate(sizes ...int) []byte {
	var out []byte
	for _, s := range sizes {
		h := make([]byte, 4)
		binary.LittleEndian.PutUint32(h, uint32(s))
		out = append(out, h...)
		out = append(out, make([]byte, s)...)
	}
	return out
}

func TestSplitterIntermediateWhole(t *testing.T) {
	ri := testRelayInit(t)
	ms, err := newMsgSplitter(ri, protoIntermediateInt)
	if err != nil {
		t.Fatal(err)
	}
	plain := buildIntermediate(8, 12) // packets of 4+8=12 and 4+12=16
	ct := encForSplitter(t, ri, plain)

	parts := ms.split(ct)
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if len(parts[0]) != 12 || len(parts[1]) != 16 {
		t.Fatalf("part lens = %d,%d want 12,16", len(parts[0]), len(parts[1]))
	}
	joined := append(append([]byte{}, parts[0]...), parts[1]...)
	if !bytes.Equal(joined, ct) {
		t.Error("parts must reconstruct the original ciphertext")
	}
}

func TestSplitterIntermediateFragmented(t *testing.T) {
	ri := testRelayInit(t)
	ms, err := newMsgSplitter(ri, protoIntermediateInt)
	if err != nil {
		t.Fatal(err)
	}
	plain := buildIntermediate(8, 12)
	ct := encForSplitter(t, ri, plain)

	// First 6 bytes: not even one full packet -> no parts yet.
	if parts := ms.split(ct[:6]); len(parts) != 0 {
		t.Fatalf("partial feed returned %d parts, want 0", len(parts))
	}
	// Feed the rest -> both packets emerge.
	parts := ms.split(ct[6:])
	if len(parts) != 2 {
		t.Fatalf("after rest got %d parts, want 2", len(parts))
	}
}

func TestSplitterAbridged(t *testing.T) {
	ri := testRelayInit(t)
	ms, err := newMsgSplitter(ri, protoAbridgedInt)
	if err != nil {
		t.Fatal(err)
	}
	// abridged: 1-byte length n (words), payload n*4 bytes. packet = 1 + n*4.
	plain := []byte{}
	plain = append(plain, 2)                   // payload 8 bytes
	plain = append(plain, make([]byte, 8)...)  //
	plain = append(plain, 3)                   // payload 12 bytes
	plain = append(plain, make([]byte, 12)...) //
	ct := encForSplitter(t, ri, plain)

	parts := ms.split(ct)
	if len(parts) != 2 || len(parts[0]) != 9 || len(parts[1]) != 13 {
		t.Fatalf("abridged parts = %v lens, want 9 and 13", lensOf(parts))
	}
}

func TestSplitterDisableOnZeroLen(t *testing.T) {
	ri := testRelayInit(t)
	ms, err := newMsgSplitter(ri, protoIntermediateInt)
	if err != nil {
		t.Fatal(err)
	}
	// A zero-length packet header makes nextPacketLen return 0 -> splitter
	// disables and passes everything through unchanged afterwards.
	plain := buildIntermediate(0)
	ct := encForSplitter(t, ri, plain)
	parts := ms.split(ct)
	if len(parts) != 1 || !bytes.Equal(parts[0], ct) {
		t.Fatalf("expected single passthrough part on zero-len")
	}
	// Now disabled: arbitrary bytes pass straight through.
	extra := []byte{9, 9, 9}
	parts = ms.split(extra)
	if len(parts) != 1 || !bytes.Equal(parts[0], extra) {
		t.Fatal("expected passthrough after disable")
	}
}

func TestSplitterDisablesOversizedPacket(t *testing.T) {
	ri := testRelayInit(t)
	ms, err := newMsgSplitter(ri, protoIntermediateInt)
	if err != nil {
		t.Fatal(err)
	}

	plain := make([]byte, 4)
	binary.LittleEndian.PutUint32(plain, maxSplitterPacketBytes)
	ct := encForSplitter(t, ri, plain)
	parts := ms.split(ct)
	if len(parts) != 1 || !bytes.Equal(parts[0], ct) {
		t.Fatal("oversized packet header must disable splitter and pass data through")
	}
	if !ms.disabled || len(ms.cipherBuf) != 0 || len(ms.plainBuf) != 0 {
		t.Fatal("oversized packet must not remain buffered")
	}
}

func TestSplitterDisablesUnrepresentableIntermediatePacket(t *testing.T) {
	ri := testRelayInit(t)
	ms, err := newMsgSplitter(ri, protoIntermediateInt)
	if err != nil {
		t.Fatal(err)
	}

	plain := make([]byte, 4)
	binary.LittleEndian.PutUint32(plain, 0x7FFFFFFF)
	ct := encForSplitter(t, ri, plain)
	parts := ms.split(ct)
	if len(parts) != 1 || !bytes.Equal(parts[0], ct) {
		t.Fatal("unrepresentable packet header must disable splitter and pass data through")
	}
}

func lensOf(parts [][]byte) []int {
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i] = len(p)
	}
	return out
}
