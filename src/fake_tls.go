package main

import (
	"context"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"time"
)

const (
	tlsRecordCCS       = 0x14
	tlsRecordHandshake = 0x16
	tlsRecordAppData   = 0x17

	tlsClientRandomOffset  = 11
	tlsClientRandomLen     = 32
	tlsSessionIDOffset     = 44
	tlsSessionIDLen        = 32
	tlsRecordMaxLen        = 16384
	tlsTimestampTolerance  = 120
	tlsGroupX25519         = 0x001d
	tlsGroupX25519MLKEM768 = 0x11ec
)

var fakeTLSCCSFrame = []byte{0x14, 0x03, 0x03, 0x00, 0x01, 0x01}

type fakeTLSClientHelloInfo struct {
	cipherSuites []uint16
	keyShares    map[uint16]int
}

type fakeTLSConn struct {
	raw     net.Conn
	readBuf []byte
}

func fakeTLSConnectLink(host string, port int, secretHex, domain string) string {
	return fmt.Sprintf(
		"tg://proxy?server=%s&port=%d&secret=ee%s%s",
		host,
		port,
		secretHex,
		hex.EncodeToString([]byte(domain)),
	)
}

func acceptFakeTLSClient(ctx context.Context, client net.Conn, secret []byte, maskingDomain string, appDataLen int, label string) (net.Conn, []byte, bool) {
	_ = client.SetReadDeadline(time.Now().Add(clientHandshakeTimeout))
	first := make([]byte, 1)
	if _, err := io.ReadFull(client, first); err != nil {
		return nil, nil, false
	}

	if first[0] != tlsRecordHandshake {
		_ = writeFakeTLSRedirect(client, maskingDomain)
		return nil, nil, false
	}

	tlsHeader := make([]byte, 5)
	tlsHeader[0] = first[0]
	if _, err := io.ReadFull(client, tlsHeader[1:]); err != nil {
		return nil, nil, false
	}
	recordLen := int(binary.BigEndian.Uint16(tlsHeader[3:5]))
	if recordLen <= 0 || recordLen > 64*1024 {
		return nil, nil, false
	}

	recordBody := make([]byte, recordLen)
	if _, err := io.ReadFull(client, recordBody); err != nil {
		return nil, nil, false
	}
	clientHello := append(tlsHeader, recordBody...)

	clientRandom, sessionID, ok := verifyFakeTLSClientHello(clientHello, secret)
	if !ok {
		log.Printf("INFO   [%s] Fake TLS verify failed -> masking", label)
		proxyToMaskingDomain(ctx, client, clientHello, maskingDomain, label)
		return nil, nil, false
	}

	serverHello, err := buildFakeTLSServerHello(secret, clientRandom, sessionID, parseFakeTLSClientHello(clientHello), appDataLen)
	if err != nil {
		log.Printf("WARN   [%s] Fake TLS server hello build failed: %v", label, err)
		return nil, nil, false
	}
	delay, err := randomIntInRange(8000, 24000)
	if err != nil {
		return nil, nil, false
	}
	timer := time.NewTimer(time.Duration(delay) * time.Microsecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, nil, false
	case <-timer.C:
	}
	_ = client.SetWriteDeadline(time.Now().Add(clientHandshakeTimeout))
	if _, err := client.Write(serverHello); err != nil {
		return nil, nil, false
	}
	_ = client.SetWriteDeadline(time.Time{})

	wrapped := &fakeTLSConn{raw: client}
	hs := make([]byte, handshakeLen)
	_ = wrapped.SetReadDeadline(time.Now().Add(clientHandshakeTimeout))
	if _, err := io.ReadFull(wrapped, hs); err != nil {
		return nil, nil, false
	}
	_ = wrapped.SetReadDeadline(time.Time{})
	return wrapped, hs, true
}

func verifyFakeTLSClientHello(data []byte, secret []byte) ([]byte, []byte, bool) {
	if len(data) < 43 {
		return nil, nil, false
	}
	if data[0] != tlsRecordHandshake || data[5] != 0x01 {
		return nil, nil, false
	}

	clientRandom := make([]byte, tlsClientRandomLen)
	copy(clientRandom, data[tlsClientRandomOffset:tlsClientRandomOffset+tlsClientRandomLen])

	zeroed := make([]byte, len(data))
	copy(zeroed, data)
	for i := 0; i < tlsClientRandomLen; i++ {
		zeroed[tlsClientRandomOffset+i] = 0
	}

	expected := hmacSHA256(secret, zeroed)
	if !hmac.Equal(expected[:28], clientRandom[:28]) {
		return nil, nil, false
	}

	var tsXor [4]byte
	for i := 0; i < 4; i++ {
		tsXor[i] = clientRandom[28+i] ^ expected[28+i]
	}
	timestamp := int64(binary.LittleEndian.Uint32(tsXor[:]))
	now := time.Now().Unix()
	if now-timestamp > tlsTimestampTolerance || timestamp-now > tlsTimestampTolerance {
		return nil, nil, false
	}

	sessionID := make([]byte, tlsSessionIDLen)
	if len(data) >= tlsSessionIDOffset+tlsSessionIDLen && data[43] == 0x20 {
		copy(sessionID, data[tlsSessionIDOffset:tlsSessionIDOffset+tlsSessionIDLen])
	}

	return clientRandom, sessionID, true
}

func parseFakeTLSClientHello(data []byte) fakeTLSClientHelloInfo {
	empty := fakeTLSClientHelloInfo{}
	if len(data) < 44 || data[0] != tlsRecordHandshake || data[5] != 0x01 ||
		int(binary.BigEndian.Uint16(data[3:5])) != len(data)-5 ||
		int(data[6])<<16|int(data[7])<<8|int(data[8]) != len(data)-9 {
		return empty
	}
	sessionLen := int(data[43])
	if sessionLen > 32 || 44+sessionLen+2 > len(data) {
		return empty
	}
	data = data[44+sessionLen:]
	suitesLen := int(binary.BigEndian.Uint16(data[:2]))
	data = data[2:]
	if suitesLen == 0 || suitesLen%2 != 0 || suitesLen >= len(data) {
		return empty
	}
	info := fakeTLSClientHelloInfo{keyShares: make(map[uint16]int)}
	for off := 0; off < suitesLen; off += 2 {
		info.cipherSuites = append(info.cipherSuites, binary.BigEndian.Uint16(data[off:off+2]))
	}
	data = data[suitesLen:]
	compressionLen := int(data[0])
	if compressionLen == 0 || 1+compressionLen+2 > len(data) {
		return empty
	}
	data = data[1+compressionLen:]
	if int(binary.BigEndian.Uint16(data[:2])) != len(data)-2 {
		return empty
	}
	data = data[2:]
	for len(data) > 0 {
		if len(data) < 4 {
			return empty
		}
		extType := binary.BigEndian.Uint16(data[:2])
		extLen := int(binary.BigEndian.Uint16(data[2:4]))
		data = data[4:]
		if extLen > len(data) {
			return empty
		}
		ext := data[:extLen]
		data = data[extLen:]
		if extType != 0x0033 {
			continue
		}
		if len(ext) < 2 || int(binary.BigEndian.Uint16(ext[:2])) != len(ext)-2 {
			return empty
		}
		for shares := ext[2:]; len(shares) > 0; {
			if len(shares) < 4 {
				return empty
			}
			group := binary.BigEndian.Uint16(shares[:2])
			keyLen := int(binary.BigEndian.Uint16(shares[2:4]))
			shares = shares[4:]
			if keyLen > len(shares) {
				return empty
			}
			if _, exists := info.keyShares[group]; !exists {
				info.keyShares[group] = keyLen
			}
			shares = shares[keyLen:]
		}
	}
	return info
}

func buildFakeTLSServerHello(secret []byte, clientRandom []byte, sessionID []byte, info fakeTLSClientHelloInfo, appDataLen int) ([]byte, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	group := uint16(tlsGroupX25519)
	keyShare := key.PublicKey().Bytes()
	if info.keyShares[tlsGroupX25519MLKEM768] == 1216 {
		group = tlsGroupX25519MLKEM768
		// Fake TLS clients authenticate the flight but do not derive TLS keys.
		ciphertext := make([]byte, 1088)
		if _, err := rand.Read(ciphertext); err != nil {
			return nil, err
		}
		keyShare = append(ciphertext, keyShare...)
	}
	cipherSuite := uint16(0x1301)
selectCipher:
	for _, preferred := range []uint16{0x1301, 0x1303, 0x1302} {
		for _, offered := range info.cipherSuites {
			if offered == preferred {
				cipherSuite = preferred
				break selectCipher
			}
		}
	}

	sh := make([]byte, 44)
	sh[0], sh[1], sh[2], sh[5], sh[9], sh[10] = tlsRecordHandshake, 0x03, 0x03, 0x02, 0x03, 0x03
	sh[43] = byte(len(sessionID))
	sh = append(sh, sessionID...)
	sh = binary.BigEndian.AppendUint16(sh, cipherSuite)
	sh = append(sh, 0x00)
	sh = binary.BigEndian.AppendUint16(sh, uint16(14+len(keyShare)))
	sh = append(sh, 0x00, 0x2b, 0x00, 0x02, 0x03, 0x04, 0x00, 0x33)
	sh = binary.BigEndian.AppendUint16(sh, uint16(4+len(keyShare)))
	sh = binary.BigEndian.AppendUint16(sh, group)
	sh = binary.BigEndian.AppendUint16(sh, uint16(len(keyShare)))
	sh = append(sh, keyShare...)
	binary.BigEndian.PutUint16(sh[3:5], uint16(len(sh)-5))
	binary.BigEndian.PutUint16(sh[7:9], uint16(len(sh)-9))

	encSize := max(64, min(appDataLen, tlsRecordMaxLen))
	if appDataLen <= 0 {
		encSize, err = randomIntInRange(64, 160)
		if err != nil {
			return nil, err
		}
	}
	encData := make([]byte, encSize)
	if _, err := rand.Read(encData); err != nil {
		return nil, err
	}

	appRecord := make([]byte, 5+encSize)
	appRecord[0] = tlsRecordAppData
	appRecord[1] = 0x03
	appRecord[2] = 0x03
	binary.BigEndian.PutUint16(appRecord[3:5], uint16(encSize))
	copy(appRecord[5:], encData)

	response := make([]byte, 0, len(sh)+len(fakeTLSCCSFrame)+len(appRecord))
	response = append(response, sh...)
	response = append(response, fakeTLSCCSFrame...)
	response = append(response, appRecord...)

	hmacInput := make([]byte, 0, len(clientRandom)+len(response))
	hmacInput = append(hmacInput, clientRandom...)
	hmacInput = append(hmacInput, response...)
	serverRandom := hmacSHA256(secret, hmacInput)
	copy(response[11:11+32], serverRandom)
	return response, nil
}

func writeFakeTLSRedirect(client net.Conn, domain string) error {
	resp := fmt.Sprintf(
		"HTTP/1.1 301 Moved Permanently\r\nLocation: https://%s/\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
		domain,
	)
	_ = client.SetWriteDeadline(time.Now().Add(fakeTLSWriteTimeout))
	_, err := client.Write([]byte(resp))
	_ = client.SetWriteDeadline(time.Time{})
	return err
}

func proxyToMaskingDomain(ctx context.Context, client net.Conn, initial []byte, domain string, label string) {
	upstream, err := newUpstreamDialer(tcpDialTimeout).DialContext(ctx, "tcp", net.JoinHostPort(domain, "443"))
	if err != nil {
		log.Printf("INFO   [%s] masking connect failed: %v", label, err)
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(ctx, func() { _ = upstream.Close(); _ = client.Close() })
	defer stop()

	log.Printf("INFO   [%s] masking -> %s:443", label, domain)
	if len(initial) > 0 {
		_ = upstream.SetWriteDeadline(time.Now().Add(fakeTLSWriteTimeout))
		if _, err := upstream.Write(initial); err != nil {
			return
		}
		_ = upstream.SetWriteDeadline(time.Time{})
	}

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
	}()
	<-done
	_ = client.Close()
	_ = upstream.Close()
	select {
	case <-done:
	case <-time.After(fakeTLSDrainGrace):
	}
}

func wrapFakeTLSRecords(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	out := make([]byte, 0, len(data)+(len(data)/tlsRecordMaxLen+1)*5)
	for len(data) > 0 {
		chunk := data
		if len(chunk) > tlsRecordMaxLen {
			chunk = chunk[:tlsRecordMaxLen]
		}
		out = append(out, tlsRecordAppData, 0x03, 0x03, 0x00, 0x00)
		binary.BigEndian.PutUint16(out[len(out)-2:], uint16(len(chunk)))
		out = append(out, chunk...)
		data = data[len(chunk):]
	}
	return out
}

func (c *fakeTLSConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if len(c.readBuf) > 0 {
		n := copy(p, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}

	payload, err := c.readTLSPayload()
	if err != nil {
		return 0, err
	}
	if len(payload) == 0 {
		return 0, io.EOF
	}

	if len(payload) > len(p) {
		n := copy(p, payload[:len(p)])
		c.readBuf = append(c.readBuf[:0], payload[len(p):]...)
		return n, nil
	}
	n := copy(p, payload)
	return n, nil
}

func (c *fakeTLSConn) readTLSPayload() ([]byte, error) {
	for {
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(c.raw, hdr); err != nil {
			return nil, err
		}
		recType := hdr[0]
		recLen := int(binary.BigEndian.Uint16(hdr[3:5]))

		if recType == tlsRecordCCS {
			if recLen > 0 {
				if _, err := io.CopyN(io.Discard, c.raw, int64(recLen)); err != nil {
					return nil, err
				}
			}
			continue
		}

		if recType != tlsRecordAppData {
			return nil, io.EOF
		}
		if recLen == 0 {
			continue
		}

		data := make([]byte, recLen)
		if _, err := io.ReadFull(c.raw, data); err != nil {
			return nil, err
		}
		return data, nil
	}
}

func (c *fakeTLSConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	framed := wrapFakeTLSRecords(p)
	if _, err := c.raw.Write(framed); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *fakeTLSConn) Close() error                       { return c.raw.Close() }
func (c *fakeTLSConn) LocalAddr() net.Addr                { return c.raw.LocalAddr() }
func (c *fakeTLSConn) RemoteAddr() net.Addr               { return c.raw.RemoteAddr() }
func (c *fakeTLSConn) SetDeadline(t time.Time) error      { return c.raw.SetDeadline(t) }
func (c *fakeTLSConn) SetReadDeadline(t time.Time) error  { return c.raw.SetReadDeadline(t) }
func (c *fakeTLSConn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }

func hmacSHA256(key []byte, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

func randomIntInRange(minVal, maxVal int) (int, error) {
	if maxVal < minVal {
		return 0, fmt.Errorf("invalid range")
	}
	width := maxVal - minVal + 1
	n, err := rand.Int(rand.Reader, big.NewInt(int64(width)))
	if err != nil {
		return 0, err
	}
	return minVal + int(n.Int64()), nil
}
