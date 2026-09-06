package main

import (
	"errors"
	"fmt"
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
	p.idle[poolKey] = []pooledWS{{Conn: conn, Created: time.Now().Add(-2 * wsPoolMaxAge)}}

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
	p.idle[poolKey] = []pooledWS{{Conn: conn, Created: time.Now()}}

	cfg := &Config{PoolSize: 0}
	if got := p.get(cfg, key, "1.2.3.4", []string{"d"}); got != conn {
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
	poolWSConnect = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
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
	poolWSConnect = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
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

func TestPoolRefillUsesFrontingDuringIPCooldown(t *testing.T) {
	origConnect := poolWSConnect
	origFrontingConnect := poolWSConnectFronting
	defer func() {
		poolWSConnect = origConnect
		poolWSConnectFronting = origFrontingConnect
		clearFrontingActive()
	}()

	const targetIP = "1.2.3.4"
	ipFuMu.Lock()
	oldCooldown, hadCooldown := ipFailUntil[targetIP]
	ipFailUntil[targetIP] = time.Now().Add(time.Hour)
	ipFuMu.Unlock()
	defer func() {
		ipFuMu.Lock()
		if hadCooldown {
			ipFailUntil[targetIP] = oldCooldown
		} else {
			delete(ipFailUntil, targetIP)
		}
		ipFuMu.Unlock()
	}()

	conn, cleanup := dialTestWS(t)
	defer cleanup()
	var frontingCalls int64
	poolWSConnect = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		t.Fatal("regular WS dial must not run during IP cooldown")
		return nil, nil, nil
	}
	poolWSConnectFronting = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		atomic.AddInt64(&frontingCalls, 1)
		return conn, nil, nil
	}

	setFrontingActive()
	p := newWSPool()
	key := wsPoolKey{DC: 1, TargetIP: targetIP}
	p.refill(&Config{PoolSize: 1}, key, []string{"d"})

	if got := atomic.LoadInt64(&frontingCalls); got != 1 {
		t.Fatalf("fronting dials = %d, want 1", got)
	}
	if got := len(p.idle[key]); got != 1 {
		t.Fatalf("pool size = %d, want 1", got)
	}
}

func TestPoolSeparatesTargets(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()

	p := newWSPool()
	key := dcKey{DC: 1}
	p.idle[wsPoolKey{DC: 1, TargetIP: "1.2.3.4"}] = []pooledWS{{Conn: conn, Created: time.Now()}}

	if got := p.get(&Config{PoolSize: 0}, key, "5.6.7.8", []string{"d"}); got != nil {
		t.Fatal("pool must not reuse a connection opened through a different target IP")
	}
}

func TestPoolDiscardsTarget(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()

	p := newWSPool()
	p.idle[wsPoolKey{DC: 1, TargetIP: "1.2.3.4"}] = []pooledWS{{Conn: conn, Created: time.Now()}}
	p.discardTarget("1.2.3.4")
	if len(p.idle) != 0 {
		t.Fatal("discardTarget must remove all pooled connections for the target IP")
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
	poolWSConnect = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		calls.Add(1)
		return fresh, nil, nil
	}
	p := newWSPool()
	key := wsPoolKey{DC: 1, TargetIP: "192.0.2.1"}
	p.routes[key] = poolRoute{domains: []string{"d"}}
	p.idle[key] = []pooledWS{{Conn: old, Created: time.Now().Add(-wsPoolMaxAge)}}
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
	orig := poolWSConnect
	defer func() { poolWSConnect = orig }()
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	var calls atomic.Int64
	var healthy atomic.Bool
	poolWSConnect = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		calls.Add(1)
		if healthy.Load() {
			return conn, nil, nil
		}
		return nil, nil, errors.New("unavailable")
	}
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

func TestPoolRotationRespectsBlockedRoutes(t *testing.T) {
	orig := poolWSConnect
	defer func() { poolWSConnect = orig }()
	var calls atomic.Int64
	poolWSConnect = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		calls.Add(1)
		return nil, nil, errors.New("unexpected dial")
	}
	p := newWSPool()
	key := wsPoolKey{DC: 1, TargetIP: "192.0.2.3"}
	p.routes[key] = poolRoute{domains: []string{"d"}}
	setIPCooldown(key.TargetIP)
	defer clearIPCooldown(key.TargetIP)
	cfg := &Config{PoolSize: 1}
	p.rotate(cfg)
	waitPoolRefill(t, p, key)
	clearIPCooldown(key.TargetIP)
	dc := dcKey{DC: key.DC}
	setBlacklisted(dc)
	defer func() { blMu.Lock(); delete(blacklist, dc); blMu.Unlock() }()
	p.rotate(cfg)
	waitPoolRefill(t, p, key)
	if calls.Load() != 0 {
		t.Fatal("rotation must not dial cooled-down or blacklisted routes")
	}
}

func TestPoolRefillRetriesFronting(t *testing.T) {
	for _, dialErr := range []error{testTimeoutError{}, fmt.Errorf("dial: %w", syscall.ECONNRESET), errors.New("other")} {
		t.Run(dialErr.Error(), func(t *testing.T) {
			orig, origFront := poolWSConnect, poolWSConnectFronting
			defer func() {
				poolWSConnect, poolWSConnectFronting = orig, origFront
				clearFrontingActive()
			}()
			conn, cleanup := dialTestWS(t)
			defer cleanup()
			var order []string
			poolWSConnect = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
				order = append(order, "direct")
				return nil, nil, dialErr
			}
			poolWSConnectFronting = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
				order = append(order, "fronting")
				return conn, nil, nil
			}
			p := newWSPool()
			key := wsPoolKey{DC: 1, TargetIP: "192.0.2.4"}
			p.refill(&Config{PoolSize: 1}, key, []string{"d"})
			want := "direct"
			if isFrontingRetryError(dialErr) {
				want = "direct,fronting"
				if !frontingActive() || len(p.idle[key]) != 1 {
					t.Fatal("successful fronting must activate fronting and refill the pool")
				}
			}
			if strings.Join(order, ",") != want {
				t.Fatalf("dial order = %v, want %s", order, want)
			}
		})
	}
}

func TestPoolConcurrentGetAndRotationShareRefill(t *testing.T) {
	orig := poolWSConnect
	defer func() { poolWSConnect = orig }()
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	var calls atomic.Int64
	release := make(chan struct{})
	poolWSConnect = func(string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
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
