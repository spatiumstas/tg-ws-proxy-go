package main

import (
	"context"
	"crypto/cipher"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	return dialWSWithSNI(targetIP, domain, domain, timeout)
}

func dialWSFronting(targetIP, domain string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	return dialWSWithSNI(targetIP, domain, "sprinthost.ru", timeout)
}

func dialWSWithSNI(targetIP, domain, sni string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	u := url.URL{Scheme: "wss", Host: domain, Path: "/apiws"}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		Subprotocols:     []string{"binary"},
		TLSClientConfig: &tls.Config{
			ServerName:         sni,
			InsecureSkipVerify: true,
		},
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newUpstreamDialer(timeout).DialContext(ctx, "tcp", net.JoinHostPort(targetIP, "443"))
		},
	}
	headers := http.Header{}
	headers.Set("Host", domain)
	headers.Set("Origin", "https://web.telegram.org")
	return dialer.Dial(u.String(), headers)
}

func writeWSBinary(ws *websocket.Conn, data []byte) error {
	if err := ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	err := ws.WriteMessage(websocket.BinaryMessage, data)
	if err == nil {
		_ = ws.SetWriteDeadline(time.Time{})
	}
	return err
}

func writeTCPRelayInit(conn net.Conn, relayInit []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	_, err := conn.Write(relayInit)
	if err == nil {
		_ = conn.SetWriteDeadline(time.Time{})
	}
	return err
}

func runIdleWatchdog(lastActivity *atomic.Int64, stop <-chan struct{}, timeout, checkInterval time.Duration, onIdle func()) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if time.Now().UnixNano()-lastActivity.Load() >= timeout.Nanoseconds() {
				onIdle()
				return
			}
		}
	}
}

func startIdleWatchdog(lastActivity *atomic.Int64, stop <-chan struct{}, onIdle func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runIdleWatchdog(lastActivity, stop, ioIdleTimeout, idleWatchdogCheckInterval, onIdle)
	}()
	return done
}

func bridgeWS(label string, cfg *Config, dc int, isMedia bool, client net.Conn, ws *websocket.Conn, cltDec, cltEnc, tgEnc, tgDec cipher.Stream, splitter *msgSplitter) {
	mediaTag := ""
	if isMedia {
		mediaTag = "m"
	}
	start := time.Now()
	var upBytes, downBytes int64
	var upPkts, downPkts int64

	done := make(chan struct{}, 2)
	stopIdleWatchdog := make(chan struct{})
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	idleWatchdogDone := startIdleWatchdog(&lastActivity, stopIdleWatchdog, func() {
		_ = ws.Close()
		_ = client.Close()
	})

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
			n, err := client.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
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
		buf := ioBufPool.Get().([]byte)
		defer ioBufPool.Put(buf)
		var downPending int64
		defer func() {
			if downPending > 0 {
				atomic.AddInt64(&stats.bytesDown, downPending)
			}
		}()
		for {
			mt, r, err := ws.NextReader()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			downPkts++
			for {
				nr, rerr := r.Read(buf)
				if nr > 0 {
					lastActivity.Store(time.Now().UnixNano())
					n := int64(nr)
					downPending += n
					downBytes += n
					if downPending >= statsFlushBytes {
						atomic.AddInt64(&stats.bytesDown, downPending)
						downPending = 0
					}
					chunk := buf[:nr]
					tgDec.XORKeyStream(chunk, chunk)
					cltEnc.XORKeyStream(chunk, chunk)
					_ = client.SetWriteDeadline(time.Now().Add(ioIdleTimeout))
					if _, werr := client.Write(chunk); werr != nil {
						return
					}
				}
				if rerr != nil {
					if rerr == io.EOF {
						break
					}
					return
				}
			}
		}
	}()

	<-done
	close(stopIdleWatchdog)
	_ = ws.Close()
	_ = client.Close()
	<-done
	<-idleWatchdogDone
	debugf(cfg, "[%s] DC%d%s WS session closed: ^%s (%d pkts) v%s (%d pkts) in %.1fs",
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
	r, err := newUpstreamDialer(tcpDialTimeout).Dial("tcp", net.JoinHostPort(dst, "443"))
	if err != nil {
		warnf("TCP fallback to %s:443 failed: %v", dst, err)
		return err
	}
	defer r.Close()
	atomic.AddInt64(&stats.connectionsTCP, 1)

	if err := writeTCPRelayInit(r, relayInit); err != nil {
		return err
	}

	done := make(chan struct{}, 2)
	stopIdleWatchdog := make(chan struct{})
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	idleWatchdogDone := startIdleWatchdog(&lastActivity, stopIdleWatchdog, func() {
		_ = client.Close()
		_ = r.Close()
	})
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
			n, err := client.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
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
			n, err := r.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
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
	close(stopIdleWatchdog)
	_ = client.Close()
	_ = r.Close()
	<-done
	<-idleWatchdogDone
	return nil
}

func wsConnect(targetIP string, domains []string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	return wsConnectWithDialer(targetIP, domains, timeout, dialWS)
}

func wsConnectFronting(targetIP string, domains []string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	return wsConnectWithDialer(targetIP, domains, timeout, dialWSFronting)
}

func wsConnectWithDialer(targetIP string, domains []string, timeout time.Duration, dial func(string, string, time.Duration) (*websocket.Conn, *http.Response, error)) (*websocket.Conn, *http.Response, error) {
	var lastErr error
	var lastResp *http.Response
	for _, domain := range domains {
		conn, resp, err := dial(targetIP, domain, timeout)
		if err == nil {
			return conn, resp, nil
		}
		lastErr = err
		lastResp = resp
		if isFrontingRetryError(err) {
			break
		}
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
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newUpstreamDialer(timeout).DialContext(ctx, network, addr)
		},
	}
	headers := http.Header{}
	headers.Set("Host", domain)
	headers.Set("Origin", "https://web.telegram.org")
	return dialer.Dial(u.String(), headers)
}

func dialWSWorker(worker, dst string, dc int, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	q := url.Values{}
	q.Set("dst", dst)
	q.Set("dc", strconv.Itoa(dc))
	u := url.URL{Scheme: "wss", Host: worker, Path: "/apiws", RawQuery: q.Encode()}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		Subprotocols:     []string{"binary"},
		TLSClientConfig: &tls.Config{
			ServerName:         worker,
			InsecureSkipVerify: true,
		},
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newUpstreamDialer(timeout).DialContext(ctx, network, addr)
		},
	}
	headers := http.Header{}
	headers.Set("Host", worker)
	headers.Set("Origin", "https://web.telegram.org")
	return dialer.Dial(u.String(), headers)
}

func cfWorkerFallback(label string, cfg *Config, dc int, isMedia bool, dst string, client net.Conn, relayInit []byte, cltDec, cltEnc, tgEnc, tgDec cipher.Stream, splitter *msgSplitter) error {
	mediaTag := ""
	if isMedia {
		mediaTag = " media"
	}
	if dst == "" {
		return errNoDomains
	}

	for _, worker := range cfg.cfproxyWorkerDomainsForTry() {
		if !cfg.beginFallbackDial(worker, true) {
			continue
		}
		logf("INFO   [%s] DC%d%s -> CF worker wss://%s/apiws?dst=%s", label, dc, mediaTag, worker, dst)
		ws, _, err := dialWSWorker(worker, dst, dc, wsConnectTimeout)
		if err != nil {
			atomic.AddInt64(&stats.wsErrors, 1)
			if cfg.finishFallbackDial(worker, true, err) {
				warnf("[%s] DC%d%s CF worker %s failed: %v", label, dc, mediaTag, worker, err)
			}
			continue
		}

		if err := writeWSBinary(ws, relayInit); err != nil {
			_ = ws.Close()
			if cfg.finishFallbackDial(worker, true, err) {
				warnf("[%s] DC%d%s CF worker init write failed: %v", label, dc, mediaTag, err)
			}
			continue
		}

		cfg.finishFallbackDial(worker, true, nil)
		atomic.AddInt64(&stats.connectionsCF, 1)
		bridgeWS(label, cfg, dc, isMedia, client, ws, cltDec, cltEnc, tgEnc, tgDec, splitter)
		return nil
	}

	return errNoDomains
}

func cfproxyFallback(label string, cfg *Config, dc int, isMedia bool, client net.Conn, relayInit []byte, cltDec, cltEnc, tgEnc, tgDec cipher.Stream, splitter *msgSplitter) error {
	mediaTag := ""
	if isMedia {
		mediaTag = " media"
	}

	attempts := 0
	for _, baseDomain := range cfg.cfproxyDomainsForTry(dc) {
		if attempts == cfProxyMaxAttempts {
			break
		}
		if !cfg.beginFallbackDial(baseDomain, false) {
			continue
		}
		attempts++
		domain := fmt.Sprintf("kws%d.%s", dc, baseDomain)
		debugf(cfg, "[%s] DC%d%s -> CF proxy wss://%s/apiws", label, dc, mediaTag, domain)
		ws, resp, err := dialWSByDomain(domain, wsConnectTimeout)
		if err != nil {
			atomic.AddInt64(&stats.wsErrors, 1)
			firstFailure := cfg.finishFallbackDial(baseDomain, false, err)
			if resp != nil && isRedirect(resp.StatusCode) {
				if firstFailure {
					warnf("[%s] DC%d%s CF proxy got %d from %s; cooling down", label, dc, mediaTag, resp.StatusCode, domain)
				}
			} else if firstFailure {
				warnf("[%s] DC%d%s CF proxy %s failed: %v", label, dc, mediaTag, domain, err)
			}
			continue
		}

		if err := writeWSBinary(ws, relayInit); err != nil {
			_ = ws.Close()
			if cfg.finishFallbackDial(baseDomain, false, err) {
				warnf("[%s] DC%d%s CF proxy init write failed: %v", label, dc, mediaTag, err)
			}
			continue
		}

		cfg.finishFallbackDial(baseDomain, false, nil)
		atomic.AddInt64(&stats.connectionsCF, 1)
		cfg.promoteCFProxyDomain(dc, baseDomain)
		bridgeWS(label, cfg, dc, isMedia, client, ws, cltDec, cltEnc, tgEnc, tgDec, splitter)
		return nil
	}

	return errNoDomains
}
