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
	for _, mode := range []string{"miss", "dead pooled connection", "disabled DC"} {
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
			dc := 99
			address := "192.0.2.99:443"
			cfg := &Config{PoolSize: 1, DCPool: map[int][]string{99: {"192.0.2.99"}}}
			if mode == "disabled DC" {
				cfg = mustParse(t, "--secret", okSecret, "--no-cfproxy", "--no-dc-ip", "2")
				dc = 2
				address = net.JoinHostPort(fallbackIP(dc), "443")
			}
			defer func() { tcpAttempts.mu.Lock(); delete(tcpAttempts.routes, address); tcpAttempts.mu.Unlock() }()
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			if mode == "dead pooled connection" {
				conn, cleanup := dialTestWS(t)
				defer cleanup()
				_ = conn.Close()
				p.idle[wsPoolKey{DC: 99, TargetIP: "192.0.2.99"}] = []pooledWS{newPooledWS(conn)}
			}
			hi := &handshakeInfo{DC: dc, ProtoTag: protoTagIntermediate, ClientDecI: make([]byte, prekeyLen+ivLen)}
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
			if mode == "disabled DC" && len(p.routes) != 0 {
				t.Fatal("disabled DC scheduled a direct WS dial")
			}
		})
	}
}

func TestClientDisconnectCancelsFallbackSetup(t *testing.T) {
	for _, worker := range []bool{false, true} {
		t.Run(map[bool]string{false: "TCP", true: "Worker"}[worker], func(t *testing.T) {
			oldPool, oldTCP, oldWorker := pool, tcpFallbackDial, workerDial
			p := newWSPool()
			pool = p
			defer func() { p.Close(); pool, tcpFallbackDial, workerDial = oldPool, oldTCP, oldWorker }()
			address := net.JoinHostPort(fallbackIP(2), "443")
			defer func() { tcpAttempts.mu.Lock(); delete(tcpAttempts.routes, address); tcpAttempts.mu.Unlock() }()
			started := make(chan struct{})
			cfg := &Config{}
			if worker {
				cfg.FallbackCFProxyWorkerDomains = []string{"worker.test"}
				workerDial = func(ctx context.Context, _, _ string, _ int, _ bool, _ time.Duration) (*websocket.Conn, *http.Response, error) {
					close(started)
					<-ctx.Done()
					return nil, nil, ctx.Err()
				}
			} else {
				tcpFallbackDial = func(ctx context.Context, _ string) (net.Conn, error) {
					close(started)
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			hi := &handshakeInfo{DC: 2, ProtoTag: protoTagIntermediate, ClientDecI: make([]byte, prekeyLen+ivLen)}
			done := make(chan struct{})
			go func() { defer close(done); handleMTProtoClient(client, cfg, hi, make([]byte, 16), "disconnect") }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("upstream dial did not start")
			}
			_ = peer.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("client disconnect left upstream dial running")
			}
			if len(cfg.cfworkerFailUntil) != 0 || len(cfg.fallbackTimeoutUntil) != 0 || len(cfg.fallbackDials) != 0 {
				t.Fatal("client cancellation cooled down a route or leaked a reservation")
			}
		})
	}
}
