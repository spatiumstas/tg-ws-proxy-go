package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const workerPoolMaxAge = 100 * time.Second

type workerPoolKey struct {
	dc  int
	dst string
}
type pooledWorker struct {
	conn    *websocket.Conn
	domain  string
	created time.Time
}
type workerPool struct {
	closeOnce sync.Once
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
	idle      map[workerPoolKey]pooledWorker
	refilling map[workerPoolKey]bool
}

var workers = newWorkerPool()

func newWorkerPool() *workerPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &workerPool{ctx: ctx, cancel: cancel, idle: make(map[workerPoolKey]pooledWorker), refilling: make(map[workerPoolKey]bool)}
}

func (p *workerPool) get(cfg *Config, key workerPoolKey) (pooledWorker, bool) {
	p.mu.Lock()
	if p.closed || cfg.PoolSize <= 0 {
		p.mu.Unlock()
		return pooledWorker{}, false
	}
	item, ok := p.idle[key]
	delete(p.idle, key)
	if ok {
		p.scheduleRefill(cfg, key)
	}
	p.mu.Unlock()
	if ok && time.Since(item.created) >= workerPoolMaxAge {
		_ = item.conn.Close()
		ok = false
	}
	if ok {
		atomic.AddInt64(&stats.workerPoolHits, 1)
	} else {
		atomic.AddInt64(&stats.workerPoolMisses, 1)
	}
	return item, ok
}

func (p *workerPool) refill(cfg *Config, key workerPoolKey) {
	p.mu.Lock()
	p.scheduleRefill(cfg, key)
	p.mu.Unlock()
}

func (p *workerPool) scheduleRefill(cfg *Config, key workerPoolKey) {
	if p.closed || cfg.PoolSize <= 0 || p.refilling[key] || !cfg.hasCFProxyWorkerDomains() {
		return
	}
	if _, ok := p.idle[key]; ok {
		return
	}
	p.refilling[key] = true
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		conn, domain, err := connectWorker(p.ctx, cfg, key.dc, key.dst, nil)
		p.mu.Lock()
		delete(p.refilling, key)
		if err == nil && !p.closed {
			p.idle[key] = pooledWorker{conn: conn, domain: domain, created: time.Now()}
			conn = nil
		}
		p.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	}()
}

func (p *workerPool) warmup(cfg *Config) {
	for dc, dst := range dcFallbackDefaults {
		if len(cfg.DCPool[dc]) == 0 {
			p.refill(cfg, workerPoolKey{dc: dc, dst: dst})
		}
	}
}

func (p *workerPool) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cancel()
		idle := p.idle
		p.idle = make(map[workerPoolKey]pooledWorker)
		p.mu.Unlock()
		for _, item := range idle {
			_ = item.conn.Close()
		}
		p.wg.Wait()
	})
}

func connectWorker(ctx context.Context, cfg *Config, dc int, dst string, relayInit []byte) (*websocket.Conn, string, error) {
	for _, domain := range cfg.cfproxyWorkerDomainsForTry() {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if !cfg.beginFallbackDial(domain, true) {
			continue
		}
		conn, _, err := workerDial(ctx, domain, dst, dc, wsConnectTimeout)
		if err == nil && relayInit != nil {
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			err = writeWSBinary(conn, relayInit)
			stop()
		}
		if ctx.Err() != nil {
			err = context.Canceled
		}
		first := cfg.finishFallbackDial(domain, true, err)
		if err == nil {
			return conn, domain, nil
		}
		if conn != nil {
			_ = conn.Close()
		}
		atomic.AddInt64(&stats.wsErrors, 1)
		if first {
			warnf("DC%d Worker %s failed: %v", dc, domain, err)
		}
	}
	return nil, "", errNoDomains
}

var workerDial = dialWSWorker
