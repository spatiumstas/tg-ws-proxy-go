package main

import (
	"context"
	"crypto/cipher"
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

func dialWebSocket(ctx context.Context, dialer *websocket.Dialer, endpoint string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	// Gorilla's HTTP upgrade read has a deadline but does not observe cancellation.
	netDial := dialer.NetDialContext
	var stop func() bool
	dialer.NetDialContext = func(dialCtx context.Context, network, addr string) (net.Conn, error) {
		conn, err := netDial(dialCtx, network, addr)
		if err == nil {
			stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
		}
		return conn, err
	}
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	return dialer.DialContext(ctx, endpoint, headers)
}

func dialWS(ctx context.Context, targetIP, domain string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	return dialWSWithSNI(ctx, targetIP, domain, domain, timeout)
}

func dialWSFronting(ctx context.Context, targetIP, domain string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	return dialWSWithSNI(ctx, targetIP, domain, "sprinthost.ru", timeout)
}

func dialWSWithSNI(ctx context.Context, targetIP, domain, sni string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	u := url.URL{Scheme: "wss", Host: domain, Path: "/apiws"}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		Subprotocols:     []string{"binary"},
		TLSClientConfig:  upstreamTLSConfig(sni, domain),
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newUpstreamDialer(timeout).DialContext(ctx, "tcp", net.JoinHostPort(targetIP, "443"))
		},
	}
	headers := http.Header{}
	headers.Set("Host", domain)
	headers.Set("Origin", "https://web.telegram.org")
	return dialWebSocket(ctx, &dialer, u.String(), headers)
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
	stop := context.AfterFunc(cfg.context(), func() { _ = ws.Close(); _ = client.Close() })
	defer stop()
	mediaTag := ""
	if isMedia {
		mediaTag = "m"
	}
	start := time.Now()
	var upBytes, downBytes int64
	var upPkts, downPkts int64
	var endOnce sync.Once
	reportEnd := func(operation string, err error) {
		endOnce.Do(func() {
			debugf(cfg, "[%s] DC%d%s WS stopping: operation=%s error=%v", label, dc, mediaTag, operation, err)
		})
	}
	debugf(cfg, "[%s] DC%d%s WS bridge started: upstream=%s splitter=%t", label, dc, mediaTag, ws.RemoteAddr(), splitter != nil)

	done := make(chan struct{}, 2)
	stopIdleWatchdog := make(chan struct{})
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var progress wsProgress
	progress.lastUp.Store(start.UnixNano())
	progress.lastDown.Store(start.UnixNano())
	progressDone := observeWSProgress(cfg, label, dc, isMedia, &progress, stopIdleWatchdog, 30*time.Second)
	idleWatchdogDone := startIdleWatchdog(&lastActivity, stopIdleWatchdog, func() {
		reportEnd("idle_timeout", nil)
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
				progress.up.Add(int64(n))
				progress.lastUp.Store(time.Now().UnixNano())
				upPkts++
				if upPending >= statsFlushBytes {
					atomic.AddInt64(&stats.bytesUp, upPending)
					upPending = 0
				}
				chunk := buf[:n]
				cltDec.XORKeyStream(chunk, chunk)

				if splitter != nil {
					parts := splitter.split(chunk)
					if len(parts) == 0 {
						continue
					}
					for _, p := range parts {
						tgEnc.XORKeyStream(p, p)
						_ = ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
						if werr := ws.WriteMessage(websocket.BinaryMessage, p); werr != nil {
							reportEnd("upstream_write", werr)
							return
						}
					}
				} else {
					tgEnc.XORKeyStream(chunk, chunk)
					_ = ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
					if werr := ws.WriteMessage(websocket.BinaryMessage, chunk); werr != nil {
						reportEnd("upstream_write", werr)
						return
					}
				}
			}
			if err != nil {
				if splitter != nil {
					reportEnd("client_read", err)
					for _, p := range splitter.flush() {
						tgEnc.XORKeyStream(p, p)
						_ = ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
						_ = ws.WriteMessage(websocket.BinaryMessage, p)
					}
				}
				reportEnd("client_read", err)
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
				reportEnd("upstream_read", err)
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
					progress.down.Add(n)
					progress.lastDown.Store(time.Now().UnixNano())
					if downPending >= statsFlushBytes {
						atomic.AddInt64(&stats.bytesDown, downPending)
						downPending = 0
					}
					chunk := buf[:nr]
					tgDec.XORKeyStream(chunk, chunk)
					cltEnc.XORKeyStream(chunk, chunk)
					_ = client.SetWriteDeadline(time.Now().Add(ioIdleTimeout))
					if _, werr := client.Write(chunk); werr != nil {
						reportEnd("client_write", werr)
						return
					}
				}
				if rerr != nil {
					if rerr == io.EOF {
						break
					}
					reportEnd("upstream_body_read", rerr)
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
	<-progressDone
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

func tcpFallback(ctx context.Context, client net.Conn, dst string, relayInit []byte, cltDec, cltEnc, tgEnc, tgDec cipher.Stream) error {
	r, err := connectTCPFallback(ctx, net.JoinHostPort(dst, "443"), relayInit)
	if err != nil {
		return err
	}
	defer r.Close()
	stop := context.AfterFunc(ctx, func() { _ = r.Close(); _ = client.Close() })
	defer stop()
	atomic.AddInt64(&stats.connectionsTCP, 1)

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

func wsConnect(ctx context.Context, targetIP string, domains []string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	return wsConnectWithDialer(ctx, targetIP, domains, timeout, dialWS)
}

func wsConnectFronting(ctx context.Context, targetIP string, domains []string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	return wsConnectWithDialer(ctx, targetIP, domains, timeout, dialWSFronting)
}

func wsConnectWithDialer(ctx context.Context, targetIP string, domains []string, timeout time.Duration, dial func(context.Context, string, string, time.Duration) (*websocket.Conn, *http.Response, error)) (*websocket.Conn, *http.Response, error) {
	var lastErr error
	var lastResp *http.Response
	for _, domain := range domains {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		conn, resp, err := dial(ctx, targetIP, domain, timeout)
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

func dialWSByDomain(ctx context.Context, domain string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	u := url.URL{Scheme: "wss", Host: domain, Path: "/apiws"}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		Subprotocols:     []string{"binary"},
		TLSClientConfig:  upstreamTLSConfig(domain, domain),
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newUpstreamDialer(timeout).DialContext(ctx, network, addr)
		},
	}
	headers := http.Header{}
	headers.Set("Host", domain)
	headers.Set("Origin", "https://web.telegram.org")
	return dialWebSocket(ctx, &dialer, u.String(), headers)
}

func dialWSWorker(ctx context.Context, worker, dst string, dc int, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	q := url.Values{}
	q.Set("dst", dst)
	q.Set("dc", strconv.Itoa(dc))
	u := url.URL{Scheme: "wss", Host: worker, Path: "/apiws", RawQuery: q.Encode()}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		Subprotocols:     []string{"binary"},
		TLSClientConfig:  upstreamTLSConfig(worker, worker),
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newUpstreamDialer(timeout).DialContext(ctx, network, addr)
		},
	}
	headers := http.Header{}
	headers.Set("Host", worker)
	headers.Set("Origin", "https://web.telegram.org")
	return dialWebSocket(ctx, &dialer, u.String(), headers)
}

func cfWorkerFallback(label string, cfg *Config, dc int, isMedia bool, dst string, client net.Conn, relayInit []byte, cltDec, cltEnc, tgEnc, tgDec cipher.Stream) error {
	if dst == "" {
		return errNoDomains
	}
	key := workerPoolKey{dc: dc, dst: dst}
	item, ok := workers.get(cfg, key)
	var ws *websocket.Conn
	if ok {
		ws = item.conn
		debugf(cfg, "[%s] DC%d Worker pool hit via %s", label, dc, item.domain)
		stop := context.AfterFunc(cfg.context(), func() { _ = item.conn.Close() })
		err := writeWSBinary(ws, relayInit)
		stop()
		if err != nil {
			_ = ws.Close()
			ws = nil
		}
	}
	if ws == nil {
		conn, _, err := connectWorker(cfg.context(), cfg, dc, dst, relayInit)
		if err != nil {
			return err
		}
		ws = conn
		workers.refill(cfg, key)
	}
	atomic.AddInt64(&stats.connectionsCF, 1)
	bridgeWS(label, cfg, dc, isMedia, client, ws, cltDec, cltEnc, tgEnc, tgDec, nil)
	return nil
}

func cfproxyFallback(label string, cfg *Config, dc int, isMedia bool, client net.Conn, relayInit []byte, cltDec, cltEnc, tgEnc, tgDec cipher.Stream, splitter *msgSplitter) error {
	mediaTag := ""
	if isMedia {
		mediaTag = " media"
	}

	attempts := 0
	for _, baseDomain := range cfg.cfproxyDomainsForTry(dc) {
		if err := cfg.context().Err(); err != nil {
			return err
		}
		if attempts == cfProxyMaxAttempts {
			break
		}
		if !cfg.beginFallbackDial(baseDomain, false) {
			continue
		}
		attempts++
		domain := fmt.Sprintf("kws%d.%s", dc, baseDomain)
		debugf(cfg, "[%s] DC%d%s -> CF proxy wss://%s/apiws", label, dc, mediaTag, domain)
		dialStart := time.Now()
		ws, resp, err := dialWSByDomain(cfg.context(), domain, wsConnectTimeout)
		debugWSDial(cfg, fmt.Sprintf("[%s] DC%d%s CF %s", label, dc, mediaTag, domain), resp, err, time.Since(dialStart))
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

		stop := context.AfterFunc(cfg.context(), func() { _ = ws.Close() })
		err = writeWSBinary(ws, relayInit)
		stop()
		if cfg.context().Err() != nil {
			err = context.Canceled
		}
		if err != nil {
			_ = ws.Close()
			if cfg.finishFallbackDial(baseDomain, false, err) {
				warnf("[%s] DC%d%s CF proxy init write failed: %v", label, dc, mediaTag, err)
			}
			continue
		}

		cfg.finishFallbackDial(baseDomain, false, nil)
		atomic.AddInt64(&stats.connectionsCF, 1)
		cfg.promoteCFProxyDomain(dc, baseDomain)
		debugf(cfg, "[%s] DC%d%s route=CF domain=%s init=ok", label, dc, mediaTag, domain)
		bridgeWS(label, cfg, dc, isMedia, client, ws, cltDec, cltEnc, tgEnc, tgDec, splitter)
		return nil
	}

	return errNoDomains
}
