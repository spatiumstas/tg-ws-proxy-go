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

func TestCFWebSocketsNoSecure(t *testing.T) {
	for _, worker := range []bool{false, true} {
		for _, noSecure := range []bool{false, true} {
			name := "CF proxy"
			if worker {
				name = "Worker"
			}
			if noSecure {
				name += "/ws"
			} else {
				name += "/wss"
			}
			t.Run(name, func(t *testing.T) {
				upgrader := websocket.Upgrader{
					Subprotocols: []string{"binary"},
					CheckOrigin:  func(r *http.Request) bool { return r.Header.Get("Origin") == "https://web.telegram.org" },
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.TLS != nil || r.URL.Path != "/apiws" || r.Header.Get("Origin") != "https://web.telegram.org" {
						t.Errorf("unexpected request: %s, TLS=%t, Origin=%s", r.URL, r.TLS != nil, r.Header.Get("Origin"))
					}
					if worker && (r.URL.Query().Get("dst") != "192.0.2.5" || r.URL.Query().Get("dc") != "5") {
						t.Errorf("wrong worker route: %s", r.URL)
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					_ = conn.WriteMessage(websocket.BinaryMessage, []byte("connected"))
				}))
				defer srv.Close()
				domain := strings.TrimPrefix(srv.URL, "http://")
				var conn *websocket.Conn
				var err error
				if worker {
					cfg := &Config{NoSecure: noSecure, FallbackCFProxyWorkerDomains: []string{domain}}
					conn, _, err = connectWorker(context.Background(), cfg, 5, "192.0.2.5", nil)
				} else {
					conn, _, err = dialWSByDomain(context.Background(), domain, noSecure, time.Second)
				}
				if conn != nil {
					defer conn.Close()
				}
				if !noSecure {
					if err == nil {
						t.Fatal("default TLS dial connected to a plaintext server")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				kind, data, err := conn.ReadMessage()
				if err != nil || kind != websocket.BinaryMessage || string(data) != "connected" || conn.Subprotocol() != "binary" {
					t.Fatalf("WebSocket exchange: kind=%d data=%q subprotocol=%q err=%v", kind, data, conn.Subprotocol(), err)
				}
			})
		}
	}
}
