package main

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestFallbackWSRetriesTimeoutAndKeepsModesIndependent(t *testing.T) {
	for _, worker := range []bool{false, true} {
		t.Run(map[bool]string{false: "CF", true: "Worker"}[worker], func(t *testing.T) {
			conn, cleanup := dialTestWS(t)
			defer cleanup()
			cfg := &Config{}
			key := fallbackDialKey{domain: "cf.example", worker: worker}
			var modes []bool
			plainTimeout := false
			dial := func(noSecure bool) (*websocket.Conn, *http.Response, error) {
				modes = append(modes, noSecure)
				if len(modes) == 1 || plainTimeout {
					return nil, nil, testTimeoutError{}
				}
				return conn, nil, nil
			}
			for range 2 {
				if got, _, err := dialFallbackWS(context.Background(), cfg, key.domain, worker, dial); err != nil || got != conn {
					t.Fatalf("HTTP fallback failed: %v", err)
				}
			}
			if !reflect.DeepEqual(modes, []bool{false, true, true}) {
				t.Fatalf("transport order: %v", modes)
			}
			plainTimeout = true
			if _, _, err := dialFallbackWS(context.Background(), cfg, key.domain, worker, dial); !isTimeoutError(err) {
				t.Fatalf("lost timeout: %v", err)
			}
			if _, _, err := dialFallbackWS(context.Background(), cfg, key.domain, worker, dial); !errors.Is(err, errFallbackCooldown) || len(modes) != 4 {
				t.Fatalf("cooled transports were retried: %v, %v", modes, err)
			}
			cfg.fallbackTimeoutUntil[fallbackModeKey{fallbackDialKey: key}] = time.Now().Add(-time.Second)
			plainTimeout = false
			if _, _, err := dialFallbackWS(context.Background(), cfg, key.domain, worker, dial); err != nil || modes[4] {
				t.Fatalf("TLS did not recover independently: %v, %v", modes, err)
			}
		})
	}
}

func TestFallbackWSOnlyDowngradesOnTimeout(t *testing.T) {
	for _, failure := range []error{websocket.ErrBadHandshake, x509.UnknownAuthorityError{}, syscall.ECONNRESET} {
		cfg := &Config{}
		calls := 0
		_, _, err := dialFallbackWS(context.Background(), cfg, "cf.example", false, func(noSecure bool) (*websocket.Conn, *http.Response, error) {
			calls++
			if noSecure {
				t.Error("non-timeout error triggered HTTP fallback")
			}
			return nil, nil, failure
		})
		if !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("unexpected retry: %v, calls=%d", err, calls)
		}
	}
	for _, worker := range []bool{false, true} {
		calls := 0
		_, _, _ = dialFallbackWS(context.Background(), &Config{NoSecure: true}, "cf.example", worker, func(noSecure bool) (*websocket.Conn, *http.Response, error) {
			calls++
			if !noSecure {
				t.Error("--no-secure attempted TLS")
			}
			return nil, nil, testTimeoutError{}
		})
		if calls != 1 {
			t.Fatalf("--no-secure attempts=%d", calls)
		}
	}
}

func TestFallbackWSCancellationClosesLateConnection(t *testing.T) {
	conn, cleanup := dialTestWS(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &Config{}
	_, _, err := dialFallbackWS(ctx, cfg, "cf.example", false, func(bool) (*websocket.Conn, *http.Response, error) {
		cancel()
		return conn, nil, nil
	})
	if !errors.Is(err, context.Canceled) || len(cfg.fallbackTimeoutUntil) != 0 {
		t.Fatalf("cancellation poisoned the route: %v", err)
	}
	if err := writeWSBinary(conn, []byte("closed")); err == nil {
		t.Fatal("late upstream connection remained open")
	}
}

func TestWorker429CooldownSharedByWarmupAndClients(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	domain := strings.TrimPrefix(srv.URL, "http://")
	cfg := &Config{PoolSize: 1, NoSecure: true, FallbackCFProxyWorkerDomains: []string{domain}}
	p := newWorkerPool()
	defer p.Close()
	p.refill(cfg, workerPoolKey{dc: 2, dst: "192.0.2.2"})
	p.wg.Wait()
	until := cfg.cfworkerRateLimitUntil[domain]
	want := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if !until.Equal(want) {
		t.Fatalf("429 cooldown ends at %v, want %v", until, want)
	}
	if _, _, err := connectWorker(context.Background(), cfg, 4, "192.0.2.4", nil); err == nil || requests.Load() != 1 {
		t.Fatalf("another DC retried exhausted Worker: requests=%d, err=%v", requests.Load(), err)
	}
	if len(cfg.fallbackDials) != 0 {
		t.Fatal("429 leaked a dial reservation")
	}
}

func TestFallbackTimeoutsDoNotHideAvailableDomains(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains([]string{"a.tld", "b.tld", "c.tld", "ready.tld"})
	for _, domain := range []string{"a.tld", "b.tld", "c.tld"} {
		_, _, err := dialFallbackWS(context.Background(), cfg, domain, false, func(bool) (*websocket.Conn, *http.Response, error) {
			return nil, nil, testTimeoutError{}
		})
		cfg.finishFallbackDial(domain, false, err)
		if cfg.beginFallbackDial(domain, false) {
			t.Fatal("cooled transports reserved a dial")
		}
	}
	if got := cfg.cfproxyDomainsForTry(2); !reflect.DeepEqual(got, []string{"ready.tld"}) {
		t.Fatalf("cooled domains consume the attempt budget: %v", got)
	}
	cfg.setCFProxyDomains([]string{"new.tld"})
	if len(cfg.fallbackTimeoutUntil) != 0 {
		t.Fatal("removed CF domains retained timeout state")
	}
}
