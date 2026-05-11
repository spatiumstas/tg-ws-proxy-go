package main

import (
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	cfRandMu sync.Mutex
	cfRand   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func defaultCFProxyDomains() []string {
	out := make([]string, 0, len(cfProxyDefaultDomainPool))
	for _, domain := range cfProxyDefaultDomainPool {
		decoded := decodeCFProxyDomain(domain)
		if normalized := normalizeCFProxyDomain(decoded); normalized != "" {
			out = appendUniqueDomains(out, normalized)
		}
	}
	return out
}

func parseCFProxyDomainCSV(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		domain := normalizeCFProxyDomain(part)
		if domain == "" {
			continue
		}
		if !isLikelyDomain(domain) {
			return nil, fmt.Errorf("invalid domain: %s", domain)
		}
		out = appendUniqueDomains(out, domain)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty domain pool")
	}
	return out, nil
}

func normalizeCFProxyDomain(raw string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(raw), "."))
}

func appendUniqueDomains(dst []string, domains ...string) []string {
	for _, domain := range domains {
		normalized := normalizeCFProxyDomain(domain)
		if normalized == "" {
			continue
		}
		duplicate := false
		for _, existing := range dst {
			if existing == normalized {
				duplicate = true
				break
			}
		}
		if !duplicate {
			dst = append(dst, normalized)
		}
	}
	return dst
}

func chooseActiveDomain(domains []string) string {
	if len(domains) == 0 {
		return ""
	}
	cfRandMu.Lock()
	defer cfRandMu.Unlock()
	return domains[cfRand.Intn(len(domains))]
}

func shuffledDomains(domains []string) []string {
	out := append([]string(nil), domains...)
	if len(out) <= 1 {
		return out
	}

	cfRandMu.Lock()
	cfRand.Shuffle(len(out), func(i, j int) {
		out[i], out[j] = out[j], out[i]
	})
	cfRandMu.Unlock()
	return out
}

func isLikelyDomain(domain string) bool {
	if len(domain) < 3 || !strings.Contains(domain, ".") {
		return false
	}
	for _, ch := range domain {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '-' {
			continue
		}
		return false
	}
	return true
}

func isValidCFProxyDomain(domain string) bool {
	d := normalizeCFProxyDomain(domain)
	if d == "" || len(d) > 253 || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return false
	}

	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}

	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, ch := range label {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				continue
			}
			return false
		}
	}

	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return false
	}
	hasLetter := false
	for _, ch := range tld {
		if ch >= 'a' && ch <= 'z' {
			hasLetter = true
			break
		}
	}
	return hasLetter
}

func decodeCFProxyDomain(raw string) string {
	s := normalizeCFProxyDomain(raw)
	if !strings.HasSuffix(s, ".com") {
		return s
	}

	base := strings.TrimSuffix(s, ".com")
	letters := 0
	for _, ch := range base {
		if ch >= 'a' && ch <= 'z' {
			letters++
		}
	}

	decoded := make([]byte, 0, len(base)+len(".co.uk"))
	for i := 0; i < len(base); i++ {
		ch := base[i]
		if ch >= 'a' && ch <= 'z' {
			shift := int(ch-'a') - letters
			shift = ((shift % 26) + 26) % 26
			decoded = append(decoded, byte('a'+shift))
			continue
		}
		decoded = append(decoded, ch)
	}
	return string(decoded) + ".co.uk"
}

func fetchCFProxyDomains(url string, timeout time.Duration) ([]string, error) {
	trimmedURL := strings.TrimSpace(url)
	if trimmedURL == "" {
		return nil, fmt.Errorf("empty cfproxy domains url")
	}
	fetchURL, err := cacheBustURL(trimmedURL)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tg-ws-proxy-go")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(body), "\n")
	accepted := 0
	pool := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		accepted++
		domain := decodeCFProxyDomain(line)
		if !isValidCFProxyDomain(domain) {
			continue
		}
		pool = appendUniqueDomains(pool, domain)
	}
	if len(pool) < defaultCFProxyRefreshMinValidDomains {
		return nil, fmt.Errorf("low-quality domain list from %s (total=%d valid=%d required>=%d)", trimmedURL, accepted, len(pool), defaultCFProxyRefreshMinValidDomains)
	}
	return pool, nil
}

func cacheBustURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}

	q := u.Query()
	q.Set("cb", fmt.Sprintf("%d", time.Now().UnixNano()))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func startCFProxyDomainRefresh(cfg *Config) {
	if cfg == nil || !cfg.FallbackCFProxy || !cfg.FallbackCFProxyRefresh || cfg.FallbackCFProxyUserDomain || strings.TrimSpace(cfg.FallbackCFProxyDomainsURL) == "" {
		return
	}
	log.Printf("INFO   CF proxy domain refresh scheduled: url=%s interval=%s", cfg.FallbackCFProxyDomainsURL, defaultCFProxyRefreshInterval)

	go func() {
		refresh := func() {
			domains, err := fetchCFProxyDomains(cfg.FallbackCFProxyDomainsURL, defaultCFProxyRefreshTimeout)
			if err != nil {
				log.Printf("WARN   CF proxy domain refresh failed: %v", err)
				return
			}
			cfg.setCFProxyDomains(domains)
			log.Printf("INFO   CF proxy domain pool updated from GitHub (%d domains): %s", len(domains), strings.Join(domains, ", "))
		}

		refresh()
		ticker := time.NewTicker(defaultCFProxyRefreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			refresh()
		}
	}()
}

func (cfg *Config) hasCFProxyDomains() bool {
	cfg.cfproxyMu.RLock()
	defer cfg.cfproxyMu.RUnlock()
	return len(cfg.FallbackCFProxyDomains) > 0
}

func (cfg *Config) cfproxyDomainsForTry(dc int) []string {
	cfg.cfproxyMu.RLock()
	defer cfg.cfproxyMu.RUnlock()

	if len(cfg.FallbackCFProxyDomains) == 0 {
		return nil
	}

	active := normalizeCFProxyDomain(cfg.FallbackCFProxyPerDCActive[dc])
	if active == "" {
		active = normalizeCFProxyDomain(cfg.FallbackCFProxyActive)
	}
	out := make([]string, 0, len(cfg.FallbackCFProxyDomains))

	if active != "" {
		for _, domain := range cfg.FallbackCFProxyDomains {
			if domain == active {
				out = append(out, domain)
				break
			}
		}
	}
	for _, domain := range shuffledDomains(cfg.FallbackCFProxyDomains) {
		if domain != active {
			out = append(out, domain)
		}
	}
	return out
}

func (cfg *Config) setCFProxyDomains(domains []string) {
	pool := make([]string, 0, len(domains))
	pool = appendUniqueDomains(pool, domains...)
	if len(pool) == 0 {
		pool = defaultCFProxyDomains()
	}
	active := chooseActiveDomain(pool)

	cfg.cfproxyMu.Lock()
	cfg.FallbackCFProxyDomains = pool
	cfg.FallbackCFProxyActive = active
	if cfg.FallbackCFProxyPerDCActive == nil {
		cfg.FallbackCFProxyPerDCActive = make(map[int]string)
	}
	for _, dc := range cfProxyKnownDCs {
		cfg.FallbackCFProxyPerDCActive[dc] = chooseActiveDomain(pool)
	}
	if active != "" {
		cfg.FallbackCFProxyDomain = active
	}
	cfg.cfproxyMu.Unlock()
}

func (cfg *Config) promoteCFProxyDomain(dc int, domain string) {
	normalized := normalizeCFProxyDomain(domain)
	if normalized == "" {
		return
	}

	cfg.cfproxyMu.Lock()
	defer cfg.cfproxyMu.Unlock()
	for _, existing := range cfg.FallbackCFProxyDomains {
		if existing == normalized {
			if cfg.FallbackCFProxyPerDCActive == nil {
				cfg.FallbackCFProxyPerDCActive = make(map[int]string)
			}
			cfg.FallbackCFProxyPerDCActive[dc] = normalized
			cfg.FallbackCFProxyActive = normalized
			cfg.FallbackCFProxyDomain = normalized
			return
		}
	}
}

func (cfg *Config) cfproxyActiveDomain() string {
	cfg.cfproxyMu.RLock()
	defer cfg.cfproxyMu.RUnlock()
	return cfg.FallbackCFProxyActive
}

func (cfg *Config) cfproxyDomainPoolSize() int {
	cfg.cfproxyMu.RLock()
	defer cfg.cfproxyMu.RUnlock()
	return len(cfg.FallbackCFProxyDomains)
}
