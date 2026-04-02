package main

import (
	"context"
	"crypto/cipher"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

func dialWS(targetIP, domain string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	u := url.URL{Scheme: "wss", Host: domain, Path: "/apiws"}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		Subprotocols:     []string{"binary"},
		TLSClientConfig: &tls.Config{
			ServerName:         domain,
			InsecureSkipVerify: true,
		},
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, "tcp", net.JoinHostPort(targetIP, "443"))
		},
	}
	headers := http.Header{}
	headers.Set("Host", domain)
	headers.Set("Origin", "https://web.telegram.org")
	headers.Set("User-Agent", "Mozilla/5.0")
	return dialer.Dial(u.String(), headers)
}

func bridgeWS(label string, dc int, isMedia bool, client net.Conn, ws *websocket.Conn, cltDec, cltEnc, tgEnc, tgDec cipher.Stream, splitter *msgSplitter) {
	mediaTag := ""
	if isMedia {
		mediaTag = "m"
	}
	start := time.Now()
	var upBytes, downBytes int64
	var upPkts, downPkts int64

	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64*1024)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				atomic.AddInt64(&stats.bytesUp, int64(n))
				upBytes += int64(n)
				upPkts++
				chunk := buf[:n]
				cltDec.XORKeyStream(chunk, chunk)
				tgEnc.XORKeyStream(chunk, chunk)

				if splitter != nil {
					parts := splitter.split(chunk)
					if len(parts) == 0 {
						continue
					}
					for _, p := range parts {
						if werr := ws.WriteMessage(websocket.BinaryMessage, p); werr != nil {
							return
						}
					}
				} else {
					if werr := ws.WriteMessage(websocket.BinaryMessage, chunk); werr != nil {
						return
					}
				}
			}
			if err != nil {
				if splitter != nil {
					for _, p := range splitter.flush() {
						_ = ws.WriteMessage(websocket.BinaryMessage, p)
					}
				}
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			atomic.AddInt64(&stats.bytesDown, int64(len(data)))
			downBytes += int64(len(data))
			downPkts++

			tgDec.XORKeyStream(data, data)
			cltEnc.XORKeyStream(data, data)
			if _, werr := client.Write(data); werr != nil {
				return
			}
		}
	}()

	<-done
	_ = ws.Close()
	_ = client.Close()
	logf("INFO   [%s] DC%d%s WS session closed: ^%s (%d pkts) v%s (%d pkts) in %.1fs",
		label,
		dc,
		mediaTag,
		humanBytes(upBytes),
		upPkts,
		humanBytes(downBytes),
		downPkts,
		time.Since(start).Seconds(),
	)
}

func tcpFallback(client net.Conn, dst string, relayInit []byte, cltDec, cltEnc, tgEnc, tgDec cipher.Stream) error {
	r, err := net.DialTimeout("tcp", net.JoinHostPort(dst, "443"), 10*time.Second)
	if err != nil {
		logf("WARNING  TCP fallback to %s:443 failed: %v", dst, err)
		return err
	}
	defer r.Close()
	atomic.AddInt64(&stats.connectionsTCP, 1)

	if _, err := r.Write(relayInit); err != nil {
		return err
	}

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64*1024)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				atomic.AddInt64(&stats.bytesUp, int64(n))
				chunk := buf[:n]
				cltDec.XORKeyStream(chunk, chunk)
				tgEnc.XORKeyStream(chunk, chunk)
				if _, werr := r.Write(chunk); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				atomic.AddInt64(&stats.bytesDown, int64(n))
				chunk := buf[:n]
				tgDec.XORKeyStream(chunk, chunk)
				cltEnc.XORKeyStream(chunk, chunk)
				if _, werr := client.Write(chunk); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	<-done
	_ = client.Close()
	_ = r.Close()
	return nil
}

func wsConnect(targetIP string, domains []string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	var lastErr error
	var lastResp *http.Response
	for _, domain := range domains {
		conn, resp, err := dialWS(targetIP, domain, timeout)
		if err == nil {
			return conn, resp, nil
		}
		lastErr = err
		lastResp = resp
	}
	if lastErr == nil {
		lastErr = errNoDomains
	}
	return nil, lastResp, lastErr
}
