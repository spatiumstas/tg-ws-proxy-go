package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPoolFallbackDoesNotWaitForDirectDial(t *testing.T) {
	for _, mode := range []string{"miss", "dead pooled connection"} {
		t.Run(mode, func(t *testing.T) {
			oldPool, oldConnect, oldTCP := pool, poolWSConnect, tcpFallbackDial
			p := newWSPool()
			pool = p
			defer func() { p.Close(); pool, poolWSConnect, tcpFallbackDial = oldPool, oldConnect, oldTCP }()
			poolWSConnect = func(ctx context.Context, _ string, _ []string, _ time.Duration) (*websocket.Conn, *http.Response, error) {
				<-ctx.Done()
				return nil, nil, ctx.Err()
			}
			var fallbackCalls atomic.Int32
			tcpFallbackDial = func(context.Context, string) (net.Conn, error) {
				fallbackCalls.Add(1)
				return nil, errors.New("offline")
			}
			const address = "192.0.2.99:443"
			defer func() { tcpAttempts.mu.Lock(); delete(tcpAttempts.routes, address); tcpAttempts.mu.Unlock() }()
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			cfg := &Config{PoolSize: 1, DCPool: map[int][]string{99: {"192.0.2.99"}}}
			if mode == "dead pooled connection" {
				conn, cleanup := dialTestWS(t)
				defer cleanup()
				_ = conn.Close()
				p.idle[wsPoolKey{DC: 99, TargetIP: "192.0.2.99"}] = []pooledWS{newPooledWS(conn)}
			}
			hi := &handshakeInfo{DC: 99, ProtoTag: protoTagIntermediate, ClientDecI: make([]byte, prekeyLen+ivLen)}
			done := make(chan struct{})
			go func() { defer close(done); handleMTProtoClient(client, cfg, hi, make([]byte, 16), "test") }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("client waited for direct dial")
			}
			if fallbackCalls.Load() != 1 {
				t.Fatalf("fallback calls = %d", fallbackCalls.Load())
			}
		})
	}
}
