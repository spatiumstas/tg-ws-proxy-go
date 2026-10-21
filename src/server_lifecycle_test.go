package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServerStopsIdleClientAndListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := &Config{ctx: ctx, SecretHex: okSecret, MaxConns: 1}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, cfg) }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("client left open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("client close timed out")
	}
}

func TestDomainRefreshCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &Config{FallbackCFProxy: true, FallbackCFProxyRefresh: true, FallbackCFProxyDomainsURL: srv.URL}
	cfg.setCFProxyDomains([]string{"keep.example"})
	done := startCFProxyDomainRefresh(ctx, cfg)
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh did not stop")
	}
	if cfg.cfproxyActiveDomain() != "keep.example" {
		t.Fatal("failed refresh erased current domains")
	}
}
