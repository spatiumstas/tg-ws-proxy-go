package main

import (
	"errors"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSConnectStopsOnRouteFailure(t *testing.T) {
	for _, firstErr := range []error{testTimeoutError{}, syscall.ECONNRESET, websocket.ErrBadHandshake} {
		t.Run(firstErr.Error(), func(t *testing.T) {
			calls := 0
			ws, _, err := wsConnectWithDialer("192.0.2.1", []string{"first", "second"}, time.Second,
				func(string, string, time.Duration) (*websocket.Conn, *http.Response, error) {
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
