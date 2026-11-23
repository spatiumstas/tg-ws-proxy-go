package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestWatchClientPreservesEarlyBytes(t *testing.T) {
	for _, fakeTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "FakeTLS"}[fakeTLS], func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			var server, peer net.Conn = a, b
			if fakeTLS {
				server, peer = &fakeTLSConn{raw: a}, &fakeTLSConn{raw: b}
			}
			ctx, client := watchClient(context.Background(), server)
			defer client.Close()
			_ = peer.SetDeadline(time.Now().Add(testIOTimeout))
			payload := bytes.Repeat([]byte("early client data"), 8192)
			written := make(chan error, 1)
			go func() {
				_, err := peer.Write(payload)
				written <- err
			}()
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(client, got); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("client bytes changed: %v", err)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil {
				t.Fatal("live client cancelled")
			}
		})
	}
}

func TestWatchClientDisconnectAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		a, b := net.Pipe()
		parent, cancel := context.WithCancel(context.Background())
		ctx, client := watchClient(parent, a)
		if shutdown {
			cancel()
		} else {
			_ = b.Close()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Error("disconnect did not cancel setup")
		}
		_ = client.Close()
		cancel()
		_ = b.Close()
	}
}
