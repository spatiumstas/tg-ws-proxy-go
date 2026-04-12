package main

import (
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

	tlsClientRandomOffset = 11
	tlsClientRandomLen    = 32
	tlsSessionIDOffset    = 44
	tlsSessionIDLen       = 32
	tlsRecordMaxLen       = 16384
	tlsTimestampTolerance = 120
)

var (
	fakeTLSCCSFrame = []byte{0x14, 0x03, 0x03, 0x00, 0x01, 0x01}
	fakeTLSServerHelloTemplate = []byte{
		0x16, 0x03, 0x03, 0x00, 0x7a,
		0x02, 0x00, 0x00, 0x76,
		0x03, 0x03,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x13, 0x01, 0x00,
		0x00, 0x2e,
		0x00, 0x33, 0x00, 0x24, 0x00, 0x1d, 0x00, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x2b, 0x00, 0x02, 0x03, 0x04,
	}
)

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

func acceptFakeTLSClient(client net.Conn, secret []byte, maskingDomain string, label string) (net.Conn, []byte, bool) {
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
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
		proxyToMaskingDomain(client, clientHello, maskingDomain, label)
		return nil, nil, false
	}

	serverHello, err := buildFakeTLSServerHello(secret, clientRandom, sessionID)
	if err != nil {
		log.Printf("WARN   [%s] Fake TLS server hello build failed: %v", label, err)
		return nil, nil, false
	}
	_ = client.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Write(serverHello); err != nil {
		return nil, nil, false
	}
	_ = client.SetWriteDeadline(time.Time{})

	wrapped := &fakeTLSConn{raw: client}
	hs := make([]byte, handshakeLen)
	_ = wrapped.SetReadDeadline(time.Now().Add(10 * time.Second))
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

func buildFakeTLSServerHello(secret []byte, clientRandom []byte, sessionID []byte) ([]byte, error) {
	sh := make([]byte, len(fakeTLSServerHelloTemplate))
	copy(sh, fakeTLSServerHelloTemplate)
	copy(sh[44:44+tlsSessionIDLen], sessionID)

	pubKey := make([]byte, 32)
	if _, err := rand.Read(pubKey); err != nil {
		return nil, err
	}
	copy(sh[89:89+32], pubKey)

	encSize, err := randomIntInRange(1900, 2100)
	if err != nil {
		return nil, err
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
	_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := client.Write([]byte(resp))
	_ = client.SetWriteDeadline(time.Time{})
	return err
}

func proxyToMaskingDomain(client net.Conn, initial []byte, domain string, label string) {
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(domain, "443"), 10*time.Second)
	if err != nil {
		log.Printf("INFO   [%s] masking connect failed: %v", label, err)
		return
	}
	defer upstream.Close()

	log.Printf("INFO   [%s] masking -> %s:443", label, domain)
	if len(initial) > 0 {
		_ = upstream.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := upstream.Write(initial); err != nil {
			return
		}
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
	case <-time.After(1 * time.Second):
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
