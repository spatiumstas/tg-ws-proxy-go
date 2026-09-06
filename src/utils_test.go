package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

type testTimeoutError struct{}

func (testTimeoutError) Error() string   { return "timeout" }
func (testTimeoutError) Timeout() bool   { return true }
func (testTimeoutError) Temporary() bool { return false }

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0.0B"},
		{512, "512.0B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1048576, "1.0MB"},
		{1073741824, "1.0GB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsRedirect(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		if !isRedirect(code) {
			t.Errorf("isRedirect(%d) = false, want true", code)
		}
	}
	for _, code := range []int{200, 204, 400, 404, 500, 0} {
		if isRedirect(code) {
			t.Errorf("isRedirect(%d) = true, want false", code)
		}
	}
}

func TestIsTimeoutError(t *testing.T) {
	if !isTimeoutError(context.DeadlineExceeded) {
		t.Fatal("context deadline should be timeout")
	}
	if !isTimeoutError(os.ErrDeadlineExceeded) {
		t.Fatal("os deadline should be timeout")
	}
	if !isTimeoutError(testTimeoutError{}) {
		t.Fatal("net.Error timeout should be timeout")
	}
	if isTimeoutError(errors.New("plain error")) {
		t.Fatal("plain error should not be timeout")
	}
	if isTimeoutError(nil) {
		t.Fatal("nil should not be timeout")
	}
}

func TestNewUpstreamDialerUsesKeepAliveConfig(t *testing.T) {
	d := newUpstreamDialer(time.Second)
	if d.Timeout != time.Second {
		t.Fatalf("timeout = %s, want 1s", d.Timeout)
	}
	if d.KeepAliveConfig != tcpKeepAliveConfig {
		t.Fatalf("keepalive config = %#v, want %#v", d.KeepAliveConfig, tcpKeepAliveConfig)
	}
}

func TestIsFrontingRetryError(t *testing.T) {
	for _, err := range []error{testTimeoutError{}, fmt.Errorf("dial: %w", syscall.ECONNRESET)} {
		if !isFrontingRetryError(err) {
			t.Errorf("expected fronting retry for %v", err)
		}
	}
	for _, err := range []error{nil, io.EOF, syscall.ECONNREFUSED, errors.New("bad handshake")} {
		if isFrontingRetryError(err) {
			t.Errorf("unexpected fronting retry for %v", err)
		}
	}
}
