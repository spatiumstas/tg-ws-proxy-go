package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type pooledWS struct {
	Conn    *websocket.Conn
	Created time.Time
}

type wsPoolKey struct {
	DC       int
	IsMedia  bool
	TargetIP string
}

type wsPool struct {
	mu        sync.Mutex
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
	return &wsPool{
		idle:      make(map[wsPoolKey][]pooledWS),
		refilling: make(map[wsPoolKey]bool),
		routes:    make(map[wsPoolKey]poolRoute),
	}
}

func (p *wsPool) get(cfg *Config, key dcKey, targetIP string, domains []string) *websocket.Conn {
	poolKey := wsPoolKey{DC: key.DC, IsMedia: key.IsMedia, TargetIP: targetIP}
	now := time.Now()
	for {
		p.mu.Lock()
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

		if now.Sub(item.Created) > wsPoolMaxAge {
			_ = item.Conn.Close()
			continue
		}
		atomic.AddInt64(&stats.poolHits, 1)
		return item.Conn
	}
}

func (p *wsPool) scheduleRefill(cfg *Config, key wsPoolKey, domains []string) {
	if cfg.PoolSize <= 0 {
		return
	}
	route, known := p.routes[key]
	if !known {
		route.domains = append([]string(nil), domains...)
		p.routes[key] = route
	}
	if p.refilling[key] || len(p.idle[key]) >= cfg.PoolSize || time.Now().Before(route.retryAfter) ||
		isBlacklisted(key.DC, key.IsMedia) || (inIPCooldown(key.TargetIP) && !frontingActive()) {
		return
	}
	p.refilling[key] = true
	go p.refill(cfg, key, domains)
}

func (p *wsPool) rotate(cfg *Config) {
	var expired []pooledWS
	now := time.Now()
	p.mu.Lock()
	for key, route := range p.routes {
		bucket := p.idle[key]
		ready := bucket[:0]
		for _, item := range bucket {
			if now.Sub(item.Created) >= wsPoolMaxAge {
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
		_ = item.Conn.Close()
	}
	if len(expired) > 0 {
		debugf(cfg, "WS pool rotated: %d expired", len(expired))
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
		p.mu.Unlock()
		if cur >= cfg.PoolSize {
			return
		}
		useFronting := frontingActive()
		if isBlacklisted(key.DC, key.IsMedia) || (inIPCooldown(key.TargetIP) && !useFronting) {
			return
		}
		connect := poolWSConnect
		if useFronting {
			connect = poolWSConnectFronting
		}
		conn, _, err := connect(key.TargetIP, domains, poolConnectTimeout)
		if err != nil && !useFronting && isFrontingRetryError(err) {
			useFronting = true
			conn, _, err = poolWSConnectFronting(key.TargetIP, domains, poolConnectTimeout)
			if err == nil {
				setFrontingActive()
				atomic.AddInt64(&stats.connectionsFront, 1)
			}
		}
		if err != nil {
			p.mu.Lock()
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
		if isBlacklisted(key.DC, key.IsMedia) || (inIPCooldown(key.TargetIP) && !useFronting) {
			p.mu.Unlock()
			_ = conn.Close()
			return
		}
		p.idle[key] = append(p.idle[key], pooledWS{Conn: conn, Created: time.Now()})
		p.routes[key] = poolRoute{domains: domains}
		p.mu.Unlock()
	}
}

func (p *wsPool) discardTarget(targetIP string) {
	if targetIP == "" {
		return
	}
	var stale []pooledWS
	p.mu.Lock()
	for key, bucket := range p.idle {
		if key.TargetIP != targetIP {
			continue
		}
		stale = append(stale, bucket...)
		delete(p.idle, key)
	}
	p.mu.Unlock()
	for _, item := range stale {
		_ = item.Conn.Close()
	}
}

func warmupPool(cfg *Config) {
	if cfg.PoolSize <= 0 {
		return
	}
	for dc, targets := range cfg.DCPool {
		if len(targets) == 0 {
			continue
		}
		ip := targets[0]
		for _, media := range []bool{false, true} {
			dcw := dc
			if v, ok := dcOverrides[dcw]; ok {
				dcw = v
			}
			key := wsPoolKey{DC: dc, IsMedia: media, TargetIP: ip}
			domains := wsDomains(dcw, media)
			pool.mu.Lock()
			pool.scheduleRefill(cfg, key, domains)
			pool.mu.Unlock()
		}
	}
	logf("INFO   WS pool warmup started for %d DC(s)", len(cfg.DCMap))
	go func() {
		ticker := time.NewTicker(wsPoolCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			pool.rotate(cfg)
		}
	}()
}
