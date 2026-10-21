package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

const (
	tcpBackoffMin = 30 * time.Second
	tcpBackoffMax = time.Hour
)

var (
	errTCPBackoff   = errors.New("TCP fallback in backoff or already connecting")
	tcpAttempts     = tcpBackoff{routes: make(map[string]tcpAttempt)}
	tcpFallbackDial = func(ctx context.Context, address string) (net.Conn, error) {
		return newUpstreamDialer(tcpDialTimeout).DialContext(ctx, "tcp", address)
	}
)

type tcpAttempt struct {
	connecting bool
	delay      time.Duration
	retryAfter time.Time
}

type tcpBackoff struct {
	mu     sync.Mutex
	routes map[string]tcpAttempt
}

func (b *tcpBackoff) begin(address string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	route := b.routes[address]
	if route.connecting || now.Before(route.retryAfter) {
		return false
	}
	route.connecting = true
	b.routes[address] = route
	return true
}

func (b *tcpBackoff) finish(address string, now time.Time, err error) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		delete(b.routes, address)
		return 0
	}
	route := b.routes[address]
	route.connecting = false
	if !errors.Is(err, context.Canceled) {
		route.delay = min(max(route.delay*2, tcpBackoffMin), tcpBackoffMax)
		route.retryAfter = now.Add(route.delay)
	}
	if route.delay == 0 {
		delete(b.routes, address)
	} else {
		b.routes[address] = route
	}
	return route.delay
}

func connectTCPFallback(ctx context.Context, address string, relayInit []byte) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !tcpAttempts.begin(address, time.Now()) {
		return nil, errTCPBackoff
	}
	logf("INFO   TCP fallback to %s", address)
	conn, err := tcpFallbackDial(ctx, address)
	if err == nil {
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		err = writeTCPRelayInit(conn, relayInit)
		stop()
	}
	if ctx.Err() != nil {
		err = context.Canceled
	}
	delay := tcpAttempts.finish(address, time.Now(), err)
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, context.Canceled) {
			warnf("TCP fallback to %s failed, retry in %s: %v", address, delay, err)
		}
		return nil, err
	}
	return conn, nil
}
