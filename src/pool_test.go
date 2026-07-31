package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
