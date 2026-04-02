package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
)

type msgSplitter struct {
	dec       cipher.Stream
	proto     uint32
	cipherBuf []byte
	plainBuf  []byte
	disabled  bool
}

func newMsgSplitter(relayInit []byte, proto uint32) (*msgSplitter, error) {
	b, err := aes.NewCipher(relayInit[8:40])
	if err != nil {
		return nil, err
	}
	dec := cipher.NewCTR(b, relayInit[40:56])
	zero := make([]byte, handshakeLen)
	tmp := make([]byte, handshakeLen)
	dec.XORKeyStream(tmp, zero)
	return &msgSplitter{dec: dec, proto: proto}, nil
}

func (m *msgSplitter) split(chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return nil
	}
	if m.disabled {
		return [][]byte{chunk}
	}

	plain := make([]byte, len(chunk))
	m.dec.XORKeyStream(plain, chunk)
	m.cipherBuf = append(m.cipherBuf, chunk...)
	m.plainBuf = append(m.plainBuf, plain...)

	parts := make([][]byte, 0, 2)
	for len(m.cipherBuf) > 0 {
		next := m.nextPacketLen()
		if next < 0 {
			break
		}
		if next == 0 {
			parts = append(parts, append([]byte(nil), m.cipherBuf...))
			m.cipherBuf = m.cipherBuf[:0]
			m.plainBuf = m.plainBuf[:0]
			m.disabled = true
			break
		}
		parts = append(parts, append([]byte(nil), m.cipherBuf[:next]...))
		m.cipherBuf = m.cipherBuf[next:]
		m.plainBuf = m.plainBuf[next:]
	}
	return parts
}

func (m *msgSplitter) flush() [][]byte {
	if len(m.cipherBuf) == 0 {
		return nil
	}
	tail := append([]byte(nil), m.cipherBuf...)
	m.cipherBuf = m.cipherBuf[:0]
	m.plainBuf = m.plainBuf[:0]
	return [][]byte{tail}
}

func (m *msgSplitter) nextPacketLen() int {
	if len(m.plainBuf) == 0 {
		return -1
	}
	switch m.proto {
	case protoAbridgedInt:
		return m.nextAbridgedLen()
	case protoIntermediateInt, protoPaddedIntermediateInt:
		return m.nextIntermediateLen()
	default:
		return 0
	}
}

func (m *msgSplitter) nextAbridgedLen() int {
	first := m.plainBuf[0]
	headerLen := 1
	payloadLen := 0
	if first == 0x7F || first == 0xFF {
		if len(m.plainBuf) < 4 {
			return -1
		}
		headerLen = 4
		payloadLen = int(uint32(m.plainBuf[1])|uint32(m.plainBuf[2])<<8|uint32(m.plainBuf[3])<<16) * 4
	} else {
		payloadLen = int(first&0x7F) * 4
	}
	if payloadLen <= 0 {
		return 0
	}
	packetLen := headerLen + payloadLen
	if len(m.plainBuf) < packetLen {
		return -1
	}
	return packetLen
}

func (m *msgSplitter) nextIntermediateLen() int {
	if len(m.plainBuf) < 4 {
		return -1
	}
	payloadLen := int(binary.LittleEndian.Uint32(m.plainBuf[:4]) & 0x7FFFFFFF)
	if payloadLen <= 0 {
		return 0
	}
	packetLen := 4 + payloadLen
	if len(m.plainBuf) < packetLen {
		return -1
	}
	return packetLen
}
