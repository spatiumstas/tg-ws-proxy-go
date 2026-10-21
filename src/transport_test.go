package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketUpgradeCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: time.Minute, NetDialContext: (&net.Dialer{}).DialContext}
	done := make(chan error, 1)
	go func() {
		conn, _, err := dialWebSocket(ctx, &dialer, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upgrade did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled upgrade succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("upgrade ignored cancellation")
	}
}

func TestWSConnectStopsOnRouteFailure(t *testing.T) {
	for _, firstErr := range []error{testTimeoutError{}, syscall.ECONNRESET, websocket.ErrBadHandshake} {
		t.Run(firstErr.Error(), func(t *testing.T) {
			calls := 0
			ws, _, err := wsConnectWithDialer(context.Background(), "192.0.2.1", []string{"first", "second"}, time.Second,
				func(context.Context, string, string, time.Duration) (*websocket.Conn, *http.Response, error) {
					calls++
					if calls == 1 {
						return nil, &http.Response{StatusCode: http.StatusFound}, firstErr
					}
					return new(websocket.Conn), nil, nil
				})
			if firstErr == websocket.ErrBadHandshake {
				if calls != 2 || ws == nil || err != nil {
					t.Fatal("HTTP redirect must allow the next domain")
				}
			} else if calls != 1 || ws != nil || !errors.Is(err, firstErr) {
				t.Fatal("timeout/reset must reach the caller without being overwritten by another domain")
			}
		})
	}
}
