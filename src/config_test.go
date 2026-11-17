package main

import "testing"

const okSecret = "00112233445566778899aabbccddeeff"

func mustParse(t *testing.T, args ...string) *Config {
	t.Helper()
	cfg, err := parseFlags(args)
	if err != nil {
		t.Fatalf("parseFlags(%v) unexpected error: %v", args, err)
	}
	return cfg
}

func wantParseErr(t *testing.T, args ...string) {
	t.Helper()
	if _, err := parseFlags(args); err == nil {
		t.Fatalf("parseFlags(%v) expected error, got nil", args)
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	cfg := mustParse(t, "-secret", okSecret)
	if cfg.Host != "127.0.0.1" || cfg.Port != 1443 {
		t.Errorf("host/port = %s:%d", cfg.Host, cfg.Port)
	}
	if cfg.PoolSize != 4 || cfg.MaxConns != defaultMaxConns {
		t.Errorf("poolSize=%d maxConns=%d", cfg.PoolSize, cfg.MaxConns)
	}
	if cfg.BufKB != 64 {
		t.Errorf("buf-kb=%d, want 64", cfg.BufKB)
	}
	if !cfg.FallbackCFProxy || !cfg.FallbackCFProxyPriority {
		t.Error("CF proxy should be enabled, CF-first by default")
	}
	if cfg.DCPool[2][0] != "149.154.167.220" || len(cfg.DCPool[4]) == 0 {
		t.Errorf("default DC pool = %v", cfg.DCPool)
	}
	if cfg.SecretHex != okSecret {
		t.Errorf("secret = %q", cfg.SecretHex)
	}
	if cfg.FallbackCFProxyDomainsURL != defaultCFProxyDomainsURL || len(cfg.FallbackCFProxyDomains) != 20 {
		t.Fatalf("CF bootstrap: URL=%q domains=%d", cfg.FallbackCFProxyDomainsURL, len(cfg.FallbackCFProxyDomains))
	}
}

func TestParseFlagsGenSecret(t *testing.T) {
	cfg := mustParse(t, "-gen-secret")
	if !cfg.GenSecret || len(cfg.SecretHex) != 32 {
		t.Errorf("gen-secret: GenSecret=%v len=%d", cfg.GenSecret, len(cfg.SecretHex))
	}
}

func TestParseFlagsSecretValidation(t *testing.T) {
	wantParseErr(t, "-secret", "tooshort")
	wantParseErr(t, "-secret", "zz112233445566778899aabbccddeeff") // 32 chars, not hex
}

func TestParseFlagsPrintLink(t *testing.T) {
	wantParseErr(t, "-print-link")
	cfg := mustParse(t, "-print-link", "-secret", okSecret)
	if !cfg.PrintLink {
		t.Error("PrintLink should be set")
	}
}

func TestParseFlagsDCIP(t *testing.T) {
	cfg := mustParse(t, "-secret", okSecret, "-dc-ip", "1:1.2.3.4")
	if len(cfg.DCPool[1]) != 1 || cfg.DCPool[1][0] != "1.2.3.4" {
		t.Errorf("DCPool[1] = %v", cfg.DCPool[1])
	}
	wantParseErr(t, "-secret", okSecret, "-dc-ip", "1.2.3.4")     // no colon
	wantParseErr(t, "-secret", okSecret, "-dc-ip", "x:1.2.3.4")   // bad dc
	wantParseErr(t, "-secret", okSecret, "-dc-ip", "1:not-an-ip") // bad ip
}

func TestParseFlagsDCIPDefault(t *testing.T) {
	wantParseErr(t, "-secret", okSecret, "-dc-ip-default", "999.999.999.999")
	cfg := mustParse(t, "-secret", okSecret, "-dc-ip-default-pool", "5.5.5.5,6.6.6.6")
	if len(cfg.DCPool[2]) != 2 {
		t.Errorf("dc-ip-default-pool should fill DC2 pool, got %v", cfg.DCPool[2])
	}
}

func TestParseFlagsNoDCIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want map[int]string
	}{
		{"DC2", []string{"--no-dc-ip", "2"}, map[int]string{4: "149.154.167.220"}},
		{"all defaults", []string{"--no-dc-ip", "2", "--no-dc-ip", "4"}, map[int]string{}},
		{"explicit routes", []string{"--no-dc-ip", "203", "--dc-ip", "203:192.0.2.1", "--dc-ip-pool", "203:192.0.2.2,192.0.2.3"}, map[int]string{2: "149.154.167.220", 4: "149.154.167.220"}},
		{"repeated DC", []string{"--no-dc-ip", "2", "--no-dc-ip", "2"}, map[int]string{4: "149.154.167.220"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustParse(t, append([]string{"--secret", okSecret}, tc.args...)...)
			if len(cfg.DCMap) != len(tc.want) || len(cfg.DCPool) != len(tc.want) {
				t.Fatalf("DCMap=%v DCPool=%v, want routes %v", cfg.DCMap, cfg.DCPool, tc.want)
			}
			for dc, ip := range tc.want {
				if cfg.DCMap[dc] != ip || len(cfg.DCPool[dc]) != 1 || cfg.DCPool[dc][0] != ip {
					t.Errorf("DC%d: map=%q pool=%v, want %s", dc, cfg.DCMap[dc], cfg.DCPool[dc], ip)
				}
			}
			if cfg.PoolSize != 4 || !cfg.FallbackCFProxy {
				t.Error("--no-dc-ip changed pool size or CF fallback")
			}
		})
	}
	for _, dc := range []string{"", "x", "0", "-2", "2:192.0.2.1"} {
		wantParseErr(t, "--secret", okSecret, "--no-dc-ip", dc)
	}
}

func TestParseFlagsCFProxy(t *testing.T) {
	wantParseErr(t, "-secret", okSecret, "-cfproxy-domain", "a.tld", "-cfproxy-domains", "b.tld")

	cfg := mustParse(t, "-secret", okSecret, "-cfproxy-domain", "mydomain.tld")
	if !cfg.FallbackCFProxyUserDomain || cfg.cfproxyActiveDomain() != "mydomain.tld" {
		t.Errorf("cfproxy-domain not applied: user=%v active=%q", cfg.FallbackCFProxyUserDomain, cfg.cfproxyActiveDomain())
	}

	cfg = mustParse(t, "-secret", okSecret, "-no-cfproxy")
	if cfg.FallbackCFProxy {
		t.Error("-no-cfproxy should disable CF proxy")
	}

	cfg = mustParse(t, "-secret", okSecret, "-cfproxy-priority=false")
	if cfg.FallbackCFProxyPriority {
		t.Error("-cfproxy-priority=false should disable CF-first")
	}
}

func TestParseFlagsWorkerDomains(t *testing.T) {
	cfg := mustParse(t, "-secret", okSecret, "-cfproxy-worker-domain", "w1.workers.dev,w2.workers.dev")
	if len(cfg.FallbackCFProxyWorkerDomains) != 2 {
		t.Errorf("worker domains = %v", cfg.FallbackCFProxyWorkerDomains)
	}
	wantParseErr(t, "-secret", okSecret, "-cfproxy-worker-domain", "not_a_domain")
}

func TestParseFlagsNoSecure(t *testing.T) {
	for _, args := range [][]string{nil, {"--no-secure"}, {"--no-secure=false"}} {
		cfg := mustParse(t, append([]string{"--secret", okSecret}, args...)...)
		want := len(args) > 0 && args[0] == "--no-secure"
		if cfg.NoSecure != want {
			t.Errorf("parseFlags(%v).NoSecure = %t, want %t", args, cfg.NoSecure, want)
		}
		if !cfg.FallbackCFProxy || cfg.FakeTLSDomain != "" {
			t.Error("--no-secure changed CF fallback or Fake TLS settings")
		}
	}
}

func TestParseFlagsFakeTLS(t *testing.T) {
	wantParseErr(t, "-secret", okSecret, "-fake-tls-domain", "nodot")
	cfg := mustParse(t, "-secret", okSecret, "-fake-tls-domain", "mask.example.com")
	if cfg.FakeTLSDomain != "mask.example.com" {
		t.Errorf("FakeTLSDomain = %q", cfg.FakeTLSDomain)
	}
}

func TestParseFlagsClamps(t *testing.T) {
	cfg := mustParse(t, "-secret", okSecret, "-buf-kb", "1", "-max-conns", "0", "-pool-size", "-5")
	if cfg.BufKB != 4 {
		t.Errorf("buf-kb clamp: %d, want 4", cfg.BufKB)
	}
	if cfg.MaxConns != 1 {
		t.Errorf("max-conns clamp: %d, want 1", cfg.MaxConns)
	}
	if cfg.PoolSize != 0 {
		t.Errorf("pool-size clamp: %d, want 0", cfg.PoolSize)
	}
}

func TestParseFlagsUnknownFlag(t *testing.T) {
	wantParseErr(t, "-secret", okSecret, "-definitely-not-a-flag")
}
