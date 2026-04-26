package main

import (
	"context"
	"crypto/cipher"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var ioBufPool = sync.Pool{
	New: func() any {
		return make([]byte, 64*1024)
	},
}

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
		buf := ioBufPool.Get().([]byte)
		defer ioBufPool.Put(buf)
		var upPending int64
		defer func() {
			if upPending > 0 {
				atomic.AddInt64(&stats.bytesUp, upPending)
			}
		}()
		for {
			_ = client.SetReadDeadline(time.Now().Add(ioIdleTimeout))
			n, err := client.Read(buf)
			if n > 0 {
				upPending += int64(n)
				upBytes += int64(n)
				upPkts++
				if upPending >= statsFlushBytes {
					atomic.AddInt64(&stats.bytesUp, upPending)
					upPending = 0
				}
				chunk := buf[:n]
				cltDec.XORKeyStream(chunk, chunk)
				tgEnc.XORKeyStream(chunk, chunk)

				if splitter != nil {
					parts := splitter.split(chunk)
					if len(parts) == 0 {
						continue
					}
					for _, p := range parts {
						_ = ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
						if werr := ws.WriteMessage(websocket.BinaryMessage, p); werr != nil {
							return
						}
					}
				} else {
					_ = ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
					if werr := ws.WriteMessage(websocket.BinaryMessage, chunk); werr != nil {
						return
					}
				}
			}
			if err != nil {
				if splitter != nil {
					for _, p := range splitter.flush() {
						_ = ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
						_ = ws.WriteMessage(websocket.BinaryMessage, p)
					}
				}
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		var downPending int64
		defer func() {
			if downPending > 0 {
				atomic.AddInt64(&stats.bytesDown, downPending)
			}
		}()
		for {
			_ = ws.SetReadDeadline(time.Now().Add(ioIdleTimeout))
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			n := int64(len(data))
			downPending += n
			downBytes += n
			downPkts++
			if downPending >= statsFlushBytes {
				atomic.AddInt64(&stats.bytesDown, downPending)
				downPending = 0
			}

			tgDec.XORKeyStream(data, data)
			cltEnc.XORKeyStream(data, data)
			_ = client.SetWriteDeadline(time.Now().Add(ioIdleTimeout))
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
		buf := ioBufPool.Get().([]byte)
		defer ioBufPool.Put(buf)
		var upPending int64
		defer func() {
			if upPending > 0 {
				atomic.AddInt64(&stats.bytesUp, upPending)
			}
		}()
		for {
			_ = client.SetReadDeadline(time.Now().Add(ioIdleTimeout))
			n, err := client.Read(buf)
			if n > 0 {
				upPending += int64(n)
				if upPending >= statsFlushBytes {
					atomic.AddInt64(&stats.bytesUp, upPending)
					upPending = 0
				}
				chunk := buf[:n]
				cltDec.XORKeyStream(chunk, chunk)
				tgEnc.XORKeyStream(chunk, chunk)
				_ = r.SetWriteDeadline(time.Now().Add(ioIdleTimeout))
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
		buf := ioBufPool.Get().([]byte)
		defer ioBufPool.Put(buf)
		var downPending int64
		defer func() {
			if downPending > 0 {
				atomic.AddInt64(&stats.bytesDown, downPending)
			}
		}()
		for {
			_ = r.SetReadDeadline(time.Now().Add(ioIdleTimeout))
			n, err := r.Read(buf)
			if n > 0 {
				downPending += int64(n)
				if downPending >= statsFlushBytes {
					atomic.AddInt64(&stats.bytesDown, downPending)
					downPending = 0
				}
				chunk := buf[:n]
				tgDec.XORKeyStream(chunk, chunk)
				cltEnc.XORKeyStream(chunk, chunk)
				_ = client.SetWriteDeadline(time.Now().Add(ioIdleTimeout))
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

func dialWSByDomain(domain string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	u := url.URL{Scheme: "wss", Host: domain, Path: "/apiws"}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		Subprotocols:     []string{"binary"},
		TLSClientConfig: &tls.Config{
			ServerName:         domain,
			InsecureSkipVerify: true,
		},
	}
	headers := http.Header{}
	headers.Set("Host", domain)
	headers.Set("Origin", "https://web.telegram.org")
	return dialer.Dial(u.String(), headers)
}

func cfproxyFallback(label string, cfg *Config, dc int, isMedia bool, client net.Conn, relayInit []byte, cltDec, cltEnc, tgEnc, tgDec cipher.Stream, splitter *msgSplitter) error {
	mediaTag := ""
	if isMedia {
		mediaTag = " media"
	}

	for _, baseDomain := range cfg.cfproxyDomainsForTry(dc) {
		domain := fmt.Sprintf("kws%d.%s", dc, baseDomain)
		logf("INFO   [%s] DC%d%s -> CF proxy wss://%s/apiws", label, dc, mediaTag, domain)
		ws, resp, err := dialWSByDomain(domain, 10*time.Second)
		if err != nil {
			atomic.AddInt64(&stats.wsErrors, 1)
			if resp != nil && isRedirect(resp.StatusCode) {
				warnf("[%s] DC%d%s CF proxy got %d from %s", label, dc, mediaTag, resp.StatusCode, domain)
			} else {
				warnf("[%s] DC%d%s CF proxy %s failed: %v", label, dc, mediaTag, domain, err)
			}
			continue
		}

		if err := ws.WriteMessage(websocket.BinaryMessage, relayInit); err != nil {
			_ = ws.Close()
			warnf("[%s] DC%d%s CF proxy init write failed: %v", label, dc, mediaTag, err)
			continue
		}

		atomic.AddInt64(&stats.connectionsCF, 1)
		cfg.promoteCFProxyDomain(dc, baseDomain)
		bridgeWS(label, dc, isMedia, client, ws, cltDec, cltEnc, tgEnc, tgDec, splitter)
		return nil
	}

	return errNoDomains
}
