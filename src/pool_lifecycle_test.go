package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPoolCloseDuringDial(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	old := poolWSConnect
	p := newWSPool()
	defer func() { p.Close(); poolWSConnect = old }()
	started := make(chan struct{})
	poolWSConnect = func(ctx context.Context, _ string, _ []string, _ time.Duration) (*websocket.Conn, *http.Response, error) {
		close(started)
		<-ctx.Done()
		return conn, nil, nil // A dial may finish at the same time as cancellation.
	}
	cfg := &Config{PoolSize: 1, DCPool: map[int][]string{2: {"192.0.2.1"}}}
	if p.getForDC(cfg, dcKey{DC: 2}) != nil {
		t.Fatal("empty pool returned a connection")
	}
	<-started
	p.Close()
	p.Close()
	if len(p.idle) != 0 || p.getForDC(cfg, dcKey{DC: 2}) != nil {
		t.Fatal("closed pool accepted a late connection")
	}
	if err := writeWSBinary(conn, []byte("closed")); err == nil {
		t.Fatal("late connection was not closed")
	}
}

func TestPoolWarmupAllIPsAndDC203(t *testing.T) {
	old := poolWSConnect
	p := newWSPool()
	defer func() { p.Close(); poolWSConnect = old }()
	type call struct {
		ip      string
		domains []string
	}
	calls := make(chan call, 4)
	poolWSConnect = func(ctx context.Context, ip string, domains []string, _ time.Duration) (*websocket.Conn, *http.Response, error) {
		calls <- call{ip, domains}
		return nil, nil, errors.New("offline")
	}
	p.warmup(&Config{PoolSize: 1, DCPool: map[int][]string{203: {"192.0.2.1", "192.0.2.2"}}})
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		select {
		case c := <-calls:
			for _, domain := range c.domains {
				if !strings.HasPrefix(domain, "kws2") {
					t.Fatalf("wrong DC mapping: %v", c.domains)
				}
			}
			seen[c.ip+strings.Join(c.domains, ",")] = true
		case <-time.After(time.Second):
			t.Fatal("warmup omitted a route")
		}
	}
	if len(seen) != 4 {
		t.Fatalf("routes = %v", seen)
	}
}

func TestPoolUsesSecondReadyIPAndDisablesDirect(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	p := newWSPool()
	defer p.Close()
	cfg := &Config{PoolSize: 1, DCPool: map[int][]string{2: {"192.0.2.1", "192.0.2.2"}}}
	for _, ip := range cfg.DCPool[2] {
		p.routes[wsPoolKey{DC: 2, TargetIP: ip}] = poolRoute{retryAfter: time.Now().Add(time.Hour)}
	}
	key := wsPoolKey{DC: 2, TargetIP: "192.0.2.2"}
	p.idle[key] = []pooledWS{{Conn: conn, Created: time.Now()}}
	cfg.PoolSize = 0
	if p.getForDC(cfg, dcKey{DC: 2}) != nil {
		t.Fatal("disabled pool returned a connection")
	}
	cfg.PoolSize = 1
	if p.getForDC(cfg, dcKey{DC: 2}) != conn {
		t.Fatal("ready second IP was ignored")
	}
}

func TestPoolRecoversFromFailedPreferredFronting(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	old, oldFront := poolWSConnect, poolWSConnectFronting
	p := newWSPool()
	defer func() { p.Close(); poolWSConnect, poolWSConnectFronting = old, oldFront; clearFrontingActive() }()
	var order []string
	poolWSConnectFronting = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		order = append(order, "front")
		return nil, nil, errors.New("offline")
	}
	poolWSConnect = func(context.Context, string, []string, time.Duration) (*websocket.Conn, *http.Response, error) {
		order = append(order, "direct")
		return conn, nil, nil
	}
	setFrontingActive()
	p.refill(&Config{PoolSize: 1}, wsPoolKey{DC: 2, TargetIP: "192.0.2.1"}, []string{"host"})
	if strings.Join(order, ",") != "front,direct" || frontingActive() {
		t.Fatalf("retry order %v", order)
	}
}

func TestWorkerPoolBoundAndClose(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "late"}[late], func(t *testing.T) {
			conn, cleanup := dialTestWS(t)
			defer cleanup()
			old := workerDial
			p := newWorkerPool()
			defer func() { p.Close(); workerDial = old }()
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			workerDial = func(ctx context.Context, domain, dst string, dc int, _ time.Duration) (*websocket.Conn, *http.Response, error) {
				if calls.Add(1) == 1 {
					close(started)
				}
				if domain != "worker.test" || dst != "192.0.2.5" || dc != 5 {
					t.Errorf("wrong worker route: %s %s %d", domain, dst, dc)
				}
				if late {
					<-ctx.Done()
				} else {
					<-release
				}
				return conn, nil, nil
			}
			cfg := &Config{PoolSize: 4, FallbackCFProxyWorkerDomains: []string{"worker.test"}}
			key := workerPoolKey{dc: 5, dst: "192.0.2.5"}
			var wg sync.WaitGroup
			for i := 0; i < 32; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); p.refill(cfg, key) }()
			}
			wg.Wait()
			<-started
			if late {
				p.Close()
			} else {
				close(release)
				p.wg.Wait()
			}
			if calls.Load() != 1 {
				t.Fatalf("dials = %d", calls.Load())
			}
			if late && len(p.idle) != 0 {
				t.Fatal("late connection retained")
			}
			if !late && len(p.idle) != 1 {
				t.Fatal("pool not refilled")
			}
			p.Close()
			if err := writeWSBinary(conn, []byte("closed")); err == nil {
				t.Fatal("worker left open")
			}
			if _, ok := p.get(cfg, key); ok {
				t.Fatal("closed worker pool returned connection")
			}
		})
	}
}

func TestWorkerPoolTTLAndDisabled(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	p := newWorkerPool()
	defer p.Close()
	key := workerPoolKey{dc: 2, dst: "192.0.2.2"}
	p.idle[key] = pooledWorker{conn: conn, created: time.Now().Add(-workerPoolMaxAge)}
	if _, ok := p.get(&Config{}, key); ok {
		t.Fatal("disabled pool returned connection")
	}
	if _, ok := p.get(&Config{PoolSize: 1}, key); ok {
		t.Fatal("expired worker returned")
	}
	if err := writeWSBinary(conn, []byte("expired")); err == nil {
		t.Fatal("expired worker not closed")
	}
}
