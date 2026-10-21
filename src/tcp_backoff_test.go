package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTCPBackoff(t *testing.T) {
	b := tcpBackoff{routes: make(map[string]tcpAttempt)}
	now := time.Now()
	for _, delay := range []time.Duration{30, 60, 120, 240, 480, 960, 1920, 3600, 3600} {
		if !b.begin("host:443", now) {
			t.Fatal("attempt rejected after backoff")
		}
		if b.begin("host:443", now) {
			t.Fatal("concurrent attempt allowed")
		}
		if got := b.finish("host:443", now, errors.New("unreachable")); got != delay*time.Second {
			t.Fatalf("delay = %s", got)
		}
		if b.begin("host:443", now.Add(delay*time.Second-time.Nanosecond)) {
			t.Fatal("backoff bypassed")
		}
		now = now.Add(delay * time.Second)
	}
	if !b.begin("host:80", now) || !b.begin("other:443", now) {
		t.Fatal("unrelated endpoint blocked")
	}
	if !b.begin("host:443", now) {
		t.Fatal("recovery blocked")
	}
	b.finish("host:443", now, nil)
	if !b.begin("host:443", now) {
		t.Fatal("success did not reset backoff")
	}
	b.finish("host:443", now, context.Canceled)
	if !b.begin("host:443", now) {
		t.Fatal("cancellation blocked endpoint")
	}
}

func TestTCPBackoffConcurrentReservation(t *testing.T) {
	b := tcpBackoff{routes: make(map[string]tcpAttempt)}
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.begin("host:443", time.Now()) {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d attempts", accepted.Load())
	}
}

func TestTCPFallbackSetup(t *testing.T) {
	for _, mode := range []string{"dial failure", "init failure", "success", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			original := tcpFallbackDial
			defer func() { tcpFallbackDial = original }()
			address := mode + ":443"
			defer func() { tcpAttempts.mu.Lock(); delete(tcpAttempts.routes, address); tcpAttempts.mu.Unlock() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var peer net.Conn
			var calls int
			tcpFallbackDial = func(context.Context, string) (net.Conn, error) {
				calls++
				if mode == "dial failure" {
					return nil, errors.New("dial failed")
				}
				c, p := net.Pipe()
				peer = p
				if mode == "init failure" {
					p.Close()
				}
				if mode == "cancel" {
					cancel()
				}
				if mode == "success" {
					go func() { _, _ = io.CopyN(io.Discard, p, 4) }()
				}
				return c, nil
			}
			conn, err := connectTCPFallback(ctx, address, []byte("init"))
			if peer != nil {
				defer peer.Close()
			}
			if conn != nil {
				defer conn.Close()
			}
			if (err == nil) != (mode == "success") {
				t.Fatalf("setup error = %v", err)
			}
			if mode == "dial failure" || mode == "init failure" {
				if _, err := connectTCPFallback(ctx, address, nil); !errors.Is(err, errTCPBackoff) {
					t.Fatalf("second error = %v", err)
				}
				if calls != 1 {
					t.Fatalf("dials = %d", calls)
				}
			}
			if mode == "success" || mode == "cancel" {
				if !tcpAttempts.begin(address, time.Now()) {
					t.Fatal("setup left endpoint blocked")
				}
			}
		})
	}
}
