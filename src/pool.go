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

type wsPool struct {
	mu        sync.Mutex
	idle      map[dcKey][]pooledWS
	refilling map[dcKey]bool
}

func newWSPool() *wsPool {
	return &wsPool{
		idle:      make(map[dcKey][]pooledWS),
		refilling: make(map[dcKey]bool),
	}
}

func (p *wsPool) get(cfg *Config, key dcKey, targetIP string, domains []string, st *Stats) *websocket.Conn {
	now := time.Now()
	p.mu.Lock()
	bucket := p.idle[key]
	for len(bucket) > 0 {
		item := bucket[0]
		bucket = bucket[1:]
		if now.Sub(item.Created) > wsPoolMaxAge {
			_ = item.Conn.Close()
			continue
		}
		p.idle[key] = bucket
		p.scheduleRefill(cfg, key, targetIP, domains)
		p.mu.Unlock()
		atomic.AddInt64(&st.poolHits, 1)
		return item.Conn
	}
	p.idle[key] = bucket
	p.scheduleRefill(cfg, key, targetIP, domains)
	p.mu.Unlock()
	atomic.AddInt64(&st.poolMisses, 1)
	return nil
}

func (p *wsPool) scheduleRefill(cfg *Config, key dcKey, targetIP string, domains []string) {
	if cfg.PoolSize <= 0 || p.refilling[key] {
		return
	}
	p.refilling[key] = true
	go p.refill(cfg, key, targetIP, domains)
}

func (p *wsPool) refill(cfg *Config, key dcKey, targetIP string, domains []string) {
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
		conn, _, err := wsConnect(targetIP, domains, 8*time.Second)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.idle[key] = append(p.idle[key], pooledWS{Conn: conn, Created: time.Now()})
		p.mu.Unlock()
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
			key := dcKey{DC: dc, IsMedia: media}
			domains := wsDomains(dcw, media)
			pool.mu.Lock()
			pool.scheduleRefill(cfg, key, ip, domains)
			pool.mu.Unlock()
		}
	}
	logf("INFO   WS pool warmup started for %d DC(s)", len(cfg.DCMap))
}
