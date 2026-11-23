package main

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSetAndTryCFProxyDomains(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains([]string{"a.tld", "b.tld", "a.tld"})

	if !cfg.hasCFProxyDomains() {
		t.Fatal("expected domains present")
	}
	if cfg.cfproxyDomainPoolSize() != 2 {
		t.Fatalf("pool size = %d, want 2 (dedup)", cfg.cfproxyDomainPoolSize())
	}
	if cfg.cfproxyActiveDomain() == "" {
		t.Fatal("active domain must be set")
	}

	order := cfg.cfproxyDomainsForTry(2)
	if len(order) != 2 {
		t.Fatalf("cfproxyDomainsForTry = %v, want 2 entries", order)
	}
	wantFirst := normalizeCFProxyDomain(cfg.FallbackCFProxyPerDCActive[2])
	if wantFirst == "" {
		wantFirst = cfg.cfproxyActiveDomain()
	}
	if order[0] != wantFirst {
		t.Errorf("per-DC active must be first: order=%v wantFirst=%q", order, wantFirst)
	}
	seen := map[string]bool{order[0]: true, order[1]: true}
	if !seen["a.tld"] || !seen["b.tld"] {
		t.Errorf("both domains must appear: %v", order)
	}
}

func TestSetCFProxyDomainsEmptyFallsBackToDefault(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains(nil)
	if !cfg.hasCFProxyDomains() {
		t.Fatal("empty input should fall back to default pool")
	}
}

func TestPromoteCFProxyDomain(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains([]string{"a.tld", "b.tld"})
	cfg.promoteCFProxyDomain(2, "b.tld")
	if cfg.cfproxyActiveDomain() != "b.tld" {
		t.Errorf("active = %q, want b.tld after promote", cfg.cfproxyActiveDomain())
	}
	// promoting an unknown domain must not change anything
	cfg.promoteCFProxyDomain(2, "zzz.tld")
	if cfg.cfproxyActiveDomain() != "b.tld" {
		t.Error("unknown domain must not be promoted")
	}
}

func TestCFProxyWorkerDomains(t *testing.T) {
	cfg := &Config{}
	if cfg.hasCFProxyWorkerDomains() {
		t.Fatal("no worker domains by default")
	}
	cfg.FallbackCFProxyWorkerDomains = []string{"w1.workers.dev", "w2.workers.dev"}
	if !cfg.hasCFProxyWorkerDomains() {
		t.Fatal("expected worker domains present")
	}
	got := cfg.cfproxyWorkerDomainsForTry()
	if len(got) != 2 {
		t.Fatalf("worker domains = %v, want 2", got)
	}
}

func TestCFProxyDomainsSkipCooldown(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains([]string{"a.tld", "b.tld", "c.tld", "d.tld"})
	cfg.promoteCFProxyDomain(2, "a.tld")
	if !cfg.markCFProxyDomainFailed("a.tld", time.Minute) {
		t.Fatal("first failure must start cooldown")
	}
	if cfg.markCFProxyDomainFailed("a.tld", time.Minute) {
		t.Fatal("second failure during cooldown must be suppressed")
	}

	order := cfg.cfproxyDomainsForTry(2)
	if len(order) != cfProxyMaxAttempts {
		t.Fatalf("cfproxyDomainsForTry returned %d domains, want %d", len(order), cfProxyMaxAttempts)
	}
	for _, domain := range order {
		if domain == "a.tld" {
			t.Fatalf("cooled-down domain was returned: %v", order)
		}
	}
}

func TestCFProxyCandidatesRemainAfterReservations(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains([]string{"a.tld", "b.tld", "c.tld", "d.tld"})
	candidates := cfg.cfproxyDomainsForTry(2)
	for _, domain := range candidates[:cfProxyMaxAttempts] {
		for range fallbackMaxDialsPerDomain {
			if !cfg.beginFallbackDial(domain, false) {
				t.Fatal("could not reserve candidate")
			}
		}
	}
	for _, domain := range candidates {
		if cfg.beginFallbackDial(domain, false) {
			cfg.finishFallbackDial(domain, false, nil)
			return
		}
	}
	t.Fatal("busy candidates hid a free domain from this session")
}

func TestFallbackDialRechecksSnapshot(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains([]string{"a.tld", "b.tld"})
	candidates := cfg.cfproxyDomainsForTry(2)
	if !cfg.beginFallbackDial(candidates[0], false) {
		t.Fatal("could not start dial")
	}
	cfg.finishFallbackDial(candidates[0], false, errors.New("dial failed"))
	if cfg.beginFallbackDial(candidates[0], false) {
		t.Fatal("stale snapshot bypassed cooldown")
	}
	cfg.setCFProxyDomains([]string{"new.tld"})
	if cfg.beginFallbackDial(candidates[1], false) {
		t.Fatal("stale snapshot allowed a removed domain")
	}
}

func TestCFProxyDomainCooldownExpiresAndSuccessClearsIt(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains([]string{"a.tld", "b.tld"})
	if !cfg.markCFProxyDomainFailed("b.tld", time.Minute) {
		t.Fatal("first failure must start cooldown")
	}
	cfg.promoteCFProxyDomain(2, "b.tld")

	cfg.cfproxyMu.RLock()
	_, stillFailed := cfg.cfproxyFailUntil["b.tld"]
	cfg.cfproxyMu.RUnlock()
	if stillFailed {
		t.Fatal("successful domain must clear cooldown")
	}

	if !cfg.markCFProxyDomainFailed("a.tld", time.Minute) {
		t.Fatal("first failure must start cooldown")
	}
	cfg.cfproxyMu.Lock()
	cfg.cfproxyFailUntil["a.tld"] = time.Now().Add(-time.Second)
	cfg.cfproxyMu.Unlock()

	order := cfg.cfproxyDomainsForTry(2)
	if !containsCFProxyDomain(order, "a.tld") {
		t.Fatalf("expired cooldown must allow domain: %v", order)
	}
}

func TestFallbackDialReservationAndCooldown(t *testing.T) {
	cfg := &Config{}
	cfg.setCFProxyDomains([]string{"a.tld"})

	var accepted int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cfg.beginFallbackDial("a.tld", false) {
				atomic.AddInt64(&accepted, 1)
			}
		}()
	}
	wg.Wait()
	if accepted != fallbackMaxDialsPerDomain {
		t.Fatalf("accepted dials = %d, want %d", accepted, fallbackMaxDialsPerDomain)
	}

	for range fallbackMaxDialsPerDomain {
		cfg.finishFallbackDial("a.tld", false, nil)
	}
	if !cfg.beginFallbackDial("a.tld", false) {
		t.Fatal("released reservation must allow another dial")
	}
	if !cfg.finishFallbackDial("a.tld", false, errors.New("dial failed")) {
		t.Fatal("first failure must start cooldown")
	}
	if cfg.beginFallbackDial("a.tld", false) {
		t.Fatal("cooled-down domain must not be reserved")
	}

	cfg.FallbackCFProxyWorkerDomains = []string{"worker.example"}
	if !cfg.beginFallbackDial("worker.example", true) {
		t.Fatal("worker domain must be reservable")
	}
	if !cfg.finishFallbackDial("worker.example", true, errors.New("worker failed")) {
		t.Fatal("worker failure must start cooldown")
	}
	if cfg.beginFallbackDial("worker.example", true) {
		t.Fatal("cooled-down worker domain must not be reserved")
	}
}

func containsCFProxyDomain(domains []string, wanted string) bool {
	for _, domain := range domains {
		if domain == wanted {
			return true
		}
	}
	return false
}

func TestWorkerRateLimitDeadline(t *testing.T) {
	now := time.Date(2026, 11, 23, 23, 30, 0, 0, time.FixedZone("MSK", 3*60*60))
	midnight := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Time
	}{
		{"", midnight},
		{"invalid", midnight},
		{"-1", midnight},
		{"99999999999999999999999999", midnight},
		{"120", now.Add(2 * time.Minute)},
		{"0", now},
		{now.Add(time.Hour).UTC().Format(http.TimeFormat), now.Add(time.Hour)},
		{now.Add(-time.Hour).UTC().Format(http.TimeFormat), now},
	} {
		t.Run(tc.header, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{"Retry-After": []string{tc.header}}}
			if got := workerRateLimitDeadline(resp, now); !got.Equal(tc.want) {
				t.Fatalf("deadline = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkerRateLimitSurvivesParallelResults(t *testing.T) {
	const domain = "worker.example"
	cfg := &Config{FallbackCFProxyWorkerDomains: []string{domain}}
	for range fallbackMaxDialsPerDomain {
		if !cfg.beginFallbackDial(domain, true) {
			t.Fatal("could not reserve concurrent dial")
		}
	}
	until := time.Now().Add(12 * time.Hour)
	if !cfg.finishFallbackDial(domain, true, &workerRateLimitError{error: errors.New("429"), until: until}) {
		t.Fatal("429 did not start cooldown")
	}
	for _, err := range []error{nil, errors.New("503"), &workerRateLimitError{error: errors.New("429"), until: time.Now().Add(time.Minute)}} {
		if cfg.finishFallbackDial(domain, true, err) || !cfg.cfworkerRateLimitUntil[domain].Equal(until) {
			t.Fatal("a late dial result shortened the rate limit")
		}
	}
	if cfg.beginFallbackDial(domain, true) || len(cfg.fallbackDials) != 0 {
		t.Fatal("exhausted Worker accepted a dial or leaked reservations")
	}
	cfg.cfworkerRateLimitUntil[domain] = time.Now().Add(-time.Second)
	if !cfg.beginFallbackDial(domain, true) {
		t.Fatal("Worker did not recover after rate limit expired")
	}
	cfg.finishFallbackDial(domain, true, nil)
}
