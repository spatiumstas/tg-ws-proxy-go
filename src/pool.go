package main

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type pooledWS struct {
	*websocket.Conn
	Created    time.Time
	first      *wsFirstRead
	firstTaken bool
}

type wsFirstRead struct {
	done   chan struct{}
	typeID int
	reader io.Reader
	err    error
}

func newPooledWS(conn *websocket.Conn) pooledWS {
	first := &wsFirstRead{done: make(chan struct{})}
	// Observe close/ping frames while idle, then hand the first data reader to the bridge.
	go func() {
		first.typeID, first.reader, first.err = conn.NextReader()
		close(first.done)
	}()
	return pooledWS{Conn: conn, Created: time.Now(), first: first}
}

func (ws *pooledWS) stale(now time.Time) bool {
	if now.Sub(ws.Created) >= wsPoolMaxAge {
		return true
	}
	select {
	case <-ws.first.done:
		return ws.first.err != nil
	default:
	}
	return false
}

func (ws *pooledWS) NextReader() (int, io.Reader, error) {
	if !ws.firstTaken {
		<-ws.first.done
		ws.firstTaken = true
		return ws.first.typeID, ws.first.reader, ws.first.err
	}
	return ws.Conn.NextReader()
}

func (ws *pooledWS) Close() error {
	err := ws.Conn.Close()
	<-ws.first.done
	return err
}

type wsPoolKey struct {
	DC       int
	IsMedia  bool
	TargetIP string
}

type wsPool struct {
	closeOnce sync.Once
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
	idle      map[wsPoolKey][]pooledWS
	refilling map[wsPoolKey]bool
	routes    map[wsPoolKey]poolRoute
}

type poolRoute struct {
	domains    []string
	retryDelay time.Duration
	retryAfter time.Time
}

var (
	poolWSConnect         = wsConnect
	poolWSConnectFronting = wsConnectFronting
)

func newWSPool() *wsPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &wsPool{
		ctx: ctx, cancel: cancel,
		idle:      make(map[wsPoolKey][]pooledWS),
		refilling: make(map[wsPoolKey]bool),
		routes:    make(map[wsPoolKey]poolRoute),
	}
}

func (p *wsPool) get(cfg *Config, key dcKey, targetIP string, domains []string) *pooledWS {
	poolKey := wsPoolKey{DC: key.DC, IsMedia: key.IsMedia, TargetIP: targetIP}
	now := time.Now()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil
		}
		bucket := p.idle[poolKey]
		if len(bucket) == 0 {
			p.scheduleRefill(cfg, poolKey, domains)
			p.mu.Unlock()
			atomic.AddInt64(&stats.poolMisses, 1)
			return nil
		}
		item := bucket[0]
		p.idle[poolKey] = bucket[1:]
		p.scheduleRefill(cfg, poolKey, domains)
		p.mu.Unlock()

		if item.stale(now) {
			_ = item.Close()
			continue
		}
		atomic.AddInt64(&stats.poolHits, 1)
		return &item
	}
}

func (p *wsPool) scheduleRefill(cfg *Config, key wsPoolKey, domains []string) {
	if p.closed || cfg.PoolSize <= 0 {
		return
	}
	route, known := p.routes[key]
	if !known {
		route.domains = append([]string(nil), domains...)
		p.routes[key] = route
	}
	if p.refilling[key] || len(p.idle[key]) >= cfg.PoolSize || time.Now().Before(route.retryAfter) {
		return
	}
	p.refilling[key] = true
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.refill(cfg, key, domains) }()
}

func (p *wsPool) rotate(cfg *Config) {
	var expired []pooledWS
	now := time.Now()
	p.mu.Lock()
	for key, route := range p.routes {
		bucket := p.idle[key]
		ready := bucket[:0]
		for _, item := range bucket {
			if item.stale(now) {
				expired = append(expired, item)
			} else {
				ready = append(ready, item)
			}
		}
		clear(bucket[len(ready):])
		p.idle[key] = ready
		p.scheduleRefill(cfg, key, route.domains)
	}
	p.mu.Unlock()
	for _, item := range expired {
		_ = item.Close()
	}
	if len(expired) > 0 {
		debugf(cfg, "WS pool rotated: %d stale", len(expired))
	}
}

func (p *wsPool) refill(cfg *Config, key wsPoolKey, domains []string) {
	defer func() {
		p.mu.Lock()
		delete(p.refilling, key)
		p.mu.Unlock()
	}()

	for {
		p.mu.Lock()
		cur := len(p.idle[key])
		closed := p.closed
		p.mu.Unlock()
		if closed || cur >= cfg.PoolSize {
			return
		}
		var conn *websocket.Conn
		err := errNoDomains
	connected:
		for _, domain := range domains {
			frontedFirst := frontingActive()
			for _, fronted := range []bool{frontedFirst, !frontedFirst} {
				if p.ctx.Err() != nil {
					return
				}
				connect := poolWSConnect
				if fronted {
					connect = poolWSConnectFronting
				}
				conn, _, err = connect(p.ctx, key.TargetIP, []string{domain}, poolConnectTimeout)
				if err != nil {
					if p.ctx.Err() != nil {
						return
					}
					atomic.AddInt64(&stats.wsErrors, 1)
					debugf(cfg, "WS pool connect %s via %s (fronting=%t) failed: %v", domain, key.TargetIP, fronted, err)
					continue
				}
				if fronted {
					setFrontingActive()
					atomic.AddInt64(&stats.connectionsFront, 1)
				} else {
					clearFrontingActive()
				}
				break connected
			}
		}
		if err != nil {
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			route := p.routes[key]
			route.domains = domains
			route.retryDelay = min(max(route.retryDelay*2, wsPoolBackoffMin), wsPoolBackoffMax)
			route.retryAfter = time.Now().Add(route.retryDelay)
			p.routes[key] = route
			p.mu.Unlock()
			debugf(cfg, "WS pool refill failed DC%d media=%t via %s, retry in %s: %v",
				key.DC, key.IsMedia, key.TargetIP, route.retryDelay, err)
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = conn.Close()
			return
		}
		p.idle[key] = append(p.idle[key], newPooledWS(conn))
		p.routes[key] = poolRoute{domains: domains}
		p.mu.Unlock()
	}
}

func (p *wsPool) getForDC(cfg *Config, key dcKey) *pooledWS {
	if cfg.PoolSize <= 0 {
		return nil
	}
	dc := key.DC
	if mapped, ok := dcOverrides[dc]; ok {
		dc = mapped
	}
	domains := wsDomains(dc, key.IsMedia)
	for _, ip := range cfg.DCPool[key.DC] {
		if ws := p.get(cfg, key, ip, domains); ws != nil {
			return ws
		}
	}
	return nil
}

func (p *wsPool) warmup(cfg *Config) {
	if cfg.PoolSize <= 0 {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	for dc, targets := range cfg.DCPool {
		dcw := dc
		if mapped, ok := dcOverrides[dc]; ok {
			dcw = mapped
		}
		for _, ip := range targets {
			for _, media := range []bool{false, true} {
				p.scheduleRefill(cfg, wsPoolKey{DC: dc, IsMedia: media, TargetIP: ip}, wsDomains(dcw, media))
			}
		}
	}
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(wsPoolCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.ctx.Done():
				return
			case <-ticker.C:
				p.rotate(cfg)
			}
		}
	}()
}

func (p *wsPool) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cancel()
		idle := p.idle
		p.idle = make(map[wsPoolKey][]pooledWS)
		p.mu.Unlock()
		for _, bucket := range idle {
			for _, item := range bucket {
				_ = item.Close()
			}
		}
		p.wg.Wait()
	})
}
