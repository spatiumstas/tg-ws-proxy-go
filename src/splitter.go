package main

import (
	"encoding/binary"
)

type msgSplitter struct {
	proto    uint32
	plainBuf []byte
	disabled bool
}

func (m *msgSplitter) split(chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return nil
	}
	if m.disabled {
		return [][]byte{chunk}
	}

	if len(m.plainBuf)+len(chunk) > maxSplitterPacketBytes {
		parts := make([][]byte, 0, 2)
		if len(m.plainBuf) > 0 {
			parts = append(parts, m.plainBuf)
		}
		parts = append(parts, chunk)
		m.plainBuf = nil
		m.disabled = true
		return parts
	}

	m.plainBuf = append(m.plainBuf, chunk...)

	parts := make([][]byte, 0, 2)
	for len(m.plainBuf) > 0 {
		next := m.nextPacketLen()
		if next < 0 {
			break
		}
		if next == 0 {
			parts = append(parts, m.plainBuf)
			m.plainBuf = m.plainBuf[:0]
			m.disabled = true
			break
		}
		parts = append(parts, m.plainBuf[:next])
		m.plainBuf = m.plainBuf[next:]
	}
	return parts
}

func (m *msgSplitter) flush() [][]byte {
	if len(m.plainBuf) == 0 {
		return nil
	}
	tail := m.plainBuf
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
	if packetLen > maxSplitterPacketBytes {
		return 0
	}
	if len(m.plainBuf) < packetLen {
		return -1
	}
	return packetLen
}

func (m *msgSplitter) nextIntermediateLen() int {
	if len(m.plainBuf) < 4 {
		return -1
	}
	payloadLen := binary.LittleEndian.Uint32(m.plainBuf[:4]) & 0x7FFFFFFF
	if payloadLen == 0 || payloadLen > maxSplitterPacketBytes-4 {
		return 0
	}
	packetLen := 4 + int(payloadLen)
	if len(m.plainBuf) < packetLen {
		return -1
	}
	return packetLen
}
