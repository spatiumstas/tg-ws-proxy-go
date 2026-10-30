package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialTestWS opens a real client WS conn to a silent local server.
func dialTestWS(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage() // block until client closes
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return conn, func() { _ = conn.Close(); srv.Close() }
}

func TestPoolDiscardsAgedConn(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()

	p := newWSPool()
	key := dcKey{DC: 1}
	poolKey := wsPoolKey{DC: key.DC, IsMedia: key.IsMedia, TargetIP: "1.2.3.4"}
	item := newPooledWS(conn)
	item.Created = time.Now().Add(-2 * wsPoolMaxAge)
	p.idle[poolKey] = []pooledWS{item}

	cfg := &Config{PoolSize: 0} // PoolSize 0 -> no background refill
	if got := p.get(cfg, key, "1.2.3.4", []string{"d"}); got != nil {
		t.Error("aged pooled conn must be discarded (get -> nil)")
	}
}

func TestPoolReturnsFreshConn(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()

	p := newWSPool()
	key := dcKey{DC: 1}
	poolKey := wsPoolKey{DC: key.DC, IsMedia: key.IsMedia, TargetIP: "1.2.3.4"}
	p.idle[poolKey] = []pooledWS{newPooledWS(conn)}

	cfg := &Config{PoolSize: 0}
	if got := p.get(cfg, key, "1.2.3.4", []string{"d"}); got == nil || got.Conn != conn {
		t.Error("fresh pooled conn must be returned as-is")
	}
}

func TestPoolRefillKeepsSizeLimit(t *testing.T) {
	origConnect := poolWSConnect
	defer func() { poolWSConnect = origConnect }()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, _, _ = c.ReadMessage()
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	var cleanupsMu sync.Mutex
	var cleanups []func()
	var calls int64
	poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		atomic.AddInt64(&calls, 1)
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			return nil, nil, err
		}
		cleanupsMu.Lock()
		cleanups = append(cleanups, func() { _ = conn.Close() })
		cleanupsMu.Unlock()
		return conn, nil, nil
	}
	defer func() {
		cleanupsMu.Lock()
		defer cleanupsMu.Unlock()
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()

	p := newWSPool()
	key := wsPoolKey{DC: 1, TargetIP: "1.2.3.4"}
	cfg := &Config{PoolSize: 4}
	p.refill(cfg, key, []string{"d"})

	if got := len(p.idle[key]); got != cfg.PoolSize {
		t.Fatalf("pool size = %d, want %d", got, cfg.PoolSize)
	}
	if got := atomic.LoadInt64(&calls); got != int64(cfg.PoolSize) {
		t.Fatalf("connect calls = %d, want %d", got, cfg.PoolSize)
	}
}

func TestPoolRefillDialsSequentially(t *testing.T) {
	origConnect := poolWSConnect
	defer func() { poolWSConnect = origConnect }()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, _, _ = c.ReadMessage()
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	var cleanupsMu sync.Mutex
	var cleanups []func()
	var active int64
	var maxActive int64
	poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		cur := atomic.AddInt64(&active, 1)
		for {
			max := atomic.LoadInt64(&maxActive)
			if cur <= max || atomic.CompareAndSwapInt64(&maxActive, max, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		atomic.AddInt64(&active, -1)
		if err != nil {
			return nil, nil, err
		}
		cleanupsMu.Lock()
		cleanups = append(cleanups, func() { _ = conn.Close() })
		cleanupsMu.Unlock()
		return conn, nil, nil
	}
	defer func() {
		cleanupsMu.Lock()
		defer cleanupsMu.Unlock()
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()

	p := newWSPool()
	key := wsPoolKey{DC: 1, TargetIP: "1.2.3.4"}
	cfg := &Config{PoolSize: 4}
	p.refill(cfg, key, []string{"d"})

	if got := atomic.LoadInt64(&maxActive); got != 1 {
		t.Fatalf("max concurrent dials = %d, want 1", got)
	}
}

func TestPoolSeparatesTargets(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()

	p := newWSPool()
	key := dcKey{DC: 1}
	p.idle[wsPoolKey{DC: 1, TargetIP: "1.2.3.4"}] = []pooledWS{newPooledWS(conn)}

	if got := p.get(&Config{PoolSize: 0}, key, "5.6.7.8", []string{"d"}); got != nil {
		t.Fatal("pool must not reuse a connection opened through a different target IP")
	}
}

func waitPoolRefill(t *testing.T, p *wsPool, key wsPoolKey) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		busy := p.refilling[key]
		p.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pool refill did not finish")
}

func TestPoolRotationReplacesExpiredWithoutClient(t *testing.T) {
	old, closeOld := dialTestWS(t)
	defer closeOld()
	fresh, closeFresh := dialTestWS(t)
	defer closeFresh()
	orig := poolWSConnect
	defer func() { poolWSConnect = orig }()
	var calls atomic.Int64
	poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		calls.Add(1)
		return fresh, nil, nil
	}
	p := newWSPool()
	key := wsPoolKey{DC: 1, TargetIP: "192.0.2.1"}
	p.routes[key] = poolRoute{domains: []string{"d"}}
	item := newPooledWS(old)
	item.Created = time.Now().Add(-wsPoolMaxAge)
	p.idle[key] = []pooledWS{item}
	cfg := &Config{PoolSize: 1}
	p.rotate(cfg)
	waitPoolRefill(t, p, key)
	if len(p.idle[key]) != 1 || p.idle[key][0].Conn != fresh {
		t.Fatal("rotation must replace expired connection without a client get")
	}
	if err := writeWSBinary(old, []byte("closed")); err == nil {
		t.Fatal("expired connection must be closed")
	}
	p.rotate(cfg)
	waitPoolRefill(t, p, key)
	if calls.Load() != 1 {
		t.Fatal("rotation must preserve fresh connections and the size limit")
	}
}

func TestPoolBackoffAndRecoveryWithoutClient(t *testing.T) {
	orig, origFront := poolWSConnect, poolWSConnectFronting
	defer func() { poolWSConnect, poolWSConnectFronting = orig, origFront; clearFrontingActive() }()
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	var calls atomic.Int64
	var healthy atomic.Bool
	poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		calls.Add(1)
		if healthy.Load() {
			return conn, nil, nil
		}
		return nil, nil, errors.New("unavailable")
	}
	poolWSConnectFronting = poolWSConnect
	p := newWSPool()
	key := wsPoolKey{DC: 1, TargetIP: "192.0.2.2"}
	cfg := &Config{PoolSize: 1}
	for _, delay := range []time.Duration{5, 10, 20, 40, 60, 60} {
		p.refill(cfg, key, []string{"d"})
		if got := p.routes[key].retryDelay; got != delay*time.Second {
			t.Fatalf("retry delay = %s, want %ds", got, delay)
		}
		before := calls.Load()
		p.get(cfg, dcKey{DC: 1}, key.TargetIP, []string{"d"})
		p.rotate(cfg)
		waitPoolRefill(t, p, key)
		if calls.Load() != before {
			t.Fatal("client get and rotation must respect backoff")
		}
	}
	healthy.Store(true)
	route := p.routes[key]
	route.retryAfter = time.Now().Add(-time.Second)
	p.routes[key] = route
	p.rotate(cfg)
	waitPoolRefill(t, p, key)
	if len(p.idle[key]) != 1 || p.idle[key][0].Conn != conn {
		t.Fatal("empty pool must recover after backoff without a client get")
	}
	if p.routes[key].retryDelay != 0 || !p.routes[key].retryAfter.IsZero() {
		t.Fatal("successful refill must reset backoff")
	}
}

func TestPoolRefillRetriesFronting(t *testing.T) {
	for _, dialErr := range []error{testTimeoutError{}, fmt.Errorf("dial: %w", syscall.ECONNRESET),
		x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"telegram.org"}}, Host: "kws2.web.telegram.org"},
		websocket.ErrBadHandshake, io.EOF, errors.New("other")} {
		t.Run(dialErr.Error(), func(t *testing.T) {
			orig, origFront := poolWSConnect, poolWSConnectFronting
			defer func() {
				poolWSConnect, poolWSConnectFronting = orig, origFront
				clearFrontingActive()
			}()
			conn, cleanup := dialTestWS(t)
			defer cleanup()
			var order []string
			poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
				order = append(order, "direct")
				return nil, nil, dialErr
			}
			poolWSConnectFronting = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
				order = append(order, "fronting")
				return conn, nil, nil
			}
			p := newWSPool()
			defer p.Close()
			key := wsPoolKey{DC: 1, TargetIP: "192.0.2.4"}
			beforeErrors := atomic.LoadInt64(&stats.wsErrors)
			p.refill(&Config{PoolSize: 1}, key, []string{"d"})
			if !frontingActive() || len(p.idle[key]) != 1 {
				t.Fatal("successful fronting must activate fronting and refill the pool")
			}
			if strings.Join(order, ",") != "direct,fronting" {
				t.Fatalf("dial order = %v, want direct,fronting", order)
			}
			if got := atomic.LoadInt64(&stats.wsErrors) - beforeErrors; got != 1 {
				t.Fatalf("failed pool dials counted = %d, want 1", got)
			}
		})
	}
}

func TestPoolDiscardsPeerClosedConn(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		t.Run(fmt.Sprintf("graceful=%t", graceful), func(t *testing.T) {
			closePeer := make(chan struct{})
			peerDone := make(chan struct{})
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(peerDone)
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				<-closePeer
				if graceful {
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "idle"), time.Now().Add(time.Second))
				}
			}))
			defer srv.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				close(closePeer)
				t.Fatal(err)
			}
			defer conn.Close()
			orig := poolWSConnect
			defer func() { poolWSConnect = orig }()
			poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
				return conn, nil, nil
			}
			p := newWSPool()
			defer p.Close()
			key := wsPoolKey{DC: 2, TargetIP: "192.0.2.1"}
			p.refill(&Config{PoolSize: 1}, key, []string{"d"})
			close(closePeer)
			<-peerDone
			if graceful {
				select {
				case <-p.idle[key][0].first.done:
				case <-time.After(time.Second):
					t.Fatal("idle reader did not observe peer closure")
				}
				if p.get(&Config{PoolSize: 0}, dcKey{DC: key.DC}, key.TargetIP, []string{"d"}) != nil {
					t.Fatal("pool handed out a closed connection")
				}
				return
			}
			deadline := time.Now().Add(time.Second)
			for len(p.idle[key]) != 0 && time.Now().Before(deadline) {
				p.rotate(&Config{PoolSize: 0})
				time.Sleep(time.Millisecond)
			}
			if len(p.idle[key]) != 0 {
				t.Fatal("pool retained a connection closed by the server")
			}
		})
	}
}

func TestPoolRefillTriesBothModesPerDomain(t *testing.T) {
	for _, frontedFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("frontedFirst=%t", frontedFirst), func(t *testing.T) {
			old, oldFront := poolWSConnect, poolWSConnectFronting
			defer func() { poolWSConnect, poolWSConnectFronting = old, oldFront; clearFrontingActive() }()
			clearFrontingActive()
			if frontedFirst {
				setFrontingActive()
			}
			conn, cleanup := dialTestWS(t)
			defer cleanup()
			var order []string
			poolWSConnect = func(_ context.Context, _ string, domains []string, _ time.Duration) (*websocket.Conn, *http.Response, error) {
				order = append(order, domains[0]+":direct")
				return nil, nil, websocket.ErrBadHandshake
			}
			poolWSConnectFronting = func(_ context.Context, _ string, domains []string, _ time.Duration) (*websocket.Conn, *http.Response, error) {
				order = append(order, domains[0]+":front")
				if domains[0] == "second" {
					return conn, nil, nil
				}
				return nil, nil, io.EOF
			}
			p := newWSPool()
			defer p.Close()
			beforeErrors, beforeFront := atomic.LoadInt64(&stats.wsErrors), atomic.LoadInt64(&stats.connectionsFront)
			p.refill(&Config{PoolSize: 1}, wsPoolKey{DC: 2, TargetIP: "192.0.2.1"}, []string{"first", "second"})
			want := "first:direct,first:front,second:direct,second:front"
			if frontedFirst {
				want = "first:front,first:direct,second:front"
			}
			if strings.Join(order, ",") != want {
				t.Fatalf("dial order = %v, want %s", order, want)
			}
			if got := atomic.LoadInt64(&stats.wsErrors) - beforeErrors; got != int64(len(order)-1) {
				t.Fatalf("errors = %d, want %d", got, len(order)-1)
			}
			if got := atomic.LoadInt64(&stats.connectionsFront) - beforeFront; got != 1 {
				t.Fatalf("fronted connections = %d, want 1", got)
			}
		})
	}
}

func TestPoolRefillCancellationStopsRetries(t *testing.T) {
	old, oldFront := poolWSConnect, poolWSConnectFronting
	defer func() { poolWSConnect, poolWSConnectFronting = old, oldFront }()
	p := newWSPool()
	defer p.Close()
	calls := 0
	poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		calls++
		p.cancel()
		return nil, nil, context.Canceled
	}
	poolWSConnectFronting = poolWSConnect
	before := atomic.LoadInt64(&stats.wsErrors)
	p.refill(&Config{PoolSize: 1}, wsPoolKey{DC: 2, TargetIP: "192.0.2.1"}, []string{"first", "second"})
	if calls != 1 || atomic.LoadInt64(&stats.wsErrors) != before {
		t.Fatalf("cancellation caused retries or an error count: calls=%d", calls)
	}
}

func TestPooledWSHandlesIdlePingAndReaderHandoff(t *testing.T) {
	pong := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		conn.SetPongHandler(func(string) error { pong <- struct{}{}; return nil })
		if err := conn.WriteControl(websocket.PingMessage, []byte("idle"), time.Now().Add(time.Second)); err != nil {
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		for _, message := range []string{"first", "second"} {
			if err := conn.WriteMessage(websocket.BinaryMessage, []byte(message)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	ws := newPooledWS(conn)
	defer ws.Close()
	select {
	case <-pong:
	case <-time.After(time.Second):
		t.Fatal("idle connection did not answer ping")
	}
	if ws.stale(time.Now()) {
		t.Fatal("live idle connection marked stale")
	}
	if err := writeWSBinary(&ws, []byte("init")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "second"} {
		mt, r, err := ws.NextReader()
		if err != nil || mt != websocket.BinaryMessage {
			t.Fatalf("NextReader = %d, %v", mt, err)
		}
		body, err := io.ReadAll(r)
		if err != nil || string(body) != want {
			t.Fatalf("message = %q, %v; want %q", body, err, want)
		}
	}
}

func TestPooledWSCloseStopsIdleReader(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	ws := newPooledWS(conn)
	done := make(chan struct{})
	go func() { _ = ws.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing pooled connection did not stop idle reader")
	}
	if !ws.stale(time.Now()) {
		t.Fatal("closed connection marked live")
	}
}

func TestPoolConcurrentGetAndRotationShareRefill(t *testing.T) {
	orig := poolWSConnect
	defer func() { poolWSConnect = orig }()
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	var calls atomic.Int64
	release := make(chan struct{})
	poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		calls.Add(1)
		<-release
		return conn, nil, nil
	}
	p := newWSPool()
	key := wsPoolKey{DC: 1, TargetIP: "192.0.2.5"}
	cfg := &Config{PoolSize: 1}
	var clients sync.WaitGroup
	for i := 0; i < 32; i++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			p.get(cfg, dcKey{DC: 1}, key.TargetIP, []string{"d"})
			p.rotate(cfg)
		}()
	}
	clients.Wait()
	close(release)
	waitPoolRefill(t, p, key)
	if calls.Load() != 1 || len(p.idle[key]) != 1 {
		t.Fatalf("concurrent get/rotation started %d dials, pool size %d", calls.Load(), len(p.idle[key]))
	}
}
