package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
)

func parseFlags() (*Config, error) {
	host := flag.String("host", "127.0.0.1", "Listen host")
	port := flag.Int("port", 1443, "Listen port")
	secret := flag.String("secret", "", "MTProto secret (32 hex chars)")
	genSecret := flag.Bool("gen-secret", false, "Generate random secret and print it")
	verbose := flag.Bool("v", false, "Verbose logs")
	logFile := flag.String("log-file", "", "Log file path")
	logMaxMB := flag.Float64("log-max-mb", 5, "Max log file size before rotate")
	logBackups := flag.Int("log-backups", 0, "Number of rotated backups")
	bufKB := flag.Int("buf-kb", 256, "Socket buffer size in KB")
	poolSize := flag.Int("pool-size", 4, "WS pool size per DC")
	fakeTLSDomain := flag.String("fake-tls-domain", "", "Enable Fake TLS (ee-secret) with masking domain")
	cfproxyDomain := flag.String("cfproxy-domain", defaultCFProxyDomain, "Cloudflare-proxied domain for WS fallback")
	cfproxyDomains := flag.String("cfproxy-domains", "", "Comma-separated Cloudflare proxy domain pool for WS fallback")
	noCfproxy := flag.Bool("no-cfproxy", false, "Disable Cloudflare proxy fallback")
	cfproxyPriority := flag.Bool("cfproxy-priority", true, "Try cfproxy before TCP fallback")
	noCfproxyDomainRefresh := flag.Bool("no-cfproxy-domain-refresh", false, "Disable periodic CF proxy domain refresh from URL")
	cfproxyDomainsURL := flag.String("cfproxy-domains-url", "", "URL to fetch CF proxy domain list from")
	maxConns := flag.Int("max-conns", defaultMaxConns, "Max concurrent client sessions")
	dcIPDefault := flag.String("dc-ip-default", "149.154.167.220", "Default WS target IP for all implicit DCs when --dc-ip is not provided")
	dcIPDefaultPool := flag.String("dc-ip-default-pool", "", "Default WS target IP pool for implicit DCs, comma-separated")
	pprofListen := flag.String("pprof-listen", "", "Optional pprof listen address (e.g. 127.0.0.1:6060)")

	var dcIPs multiFlag
	var dcIPPools multiFlag
	flag.Var(&dcIPs, "dc-ip", "Target DC IP as DC:IP; repeatable")
	flag.Var(&dcIPPools, "dc-ip-pool", "Target pool as DC:IP1,IP2,...; repeatable")
	flag.Parse()

	if *secret == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		*secret = hex.EncodeToString(b)
		if !*genSecret {
			log.Printf("INFO   Generated secret: %s", *secret)
		}
	}
	if len(*secret) != 32 {
		return nil, errors.New("secret must be exactly 32 hex chars")
	}
	if _, err := hex.DecodeString(*secret); err != nil {
		return nil, errors.New("secret must be valid hex")
	}

	defaultTargetIP := strings.TrimSpace(*dcIPDefault)
	if net.ParseIP(defaultTargetIP) == nil {
		return nil, fmt.Errorf("invalid --dc-ip-default: %s", defaultTargetIP)
	}

	defaultPool := []string{defaultTargetIP}
	if strings.TrimSpace(*dcIPDefaultPool) != "" {
		poolIPs, err := parseIPCSV(*dcIPDefaultPool)
		if err != nil {
			return nil, fmt.Errorf("invalid --dc-ip-default-pool: %w", err)
		}
		defaultPool = poolIPs
	}

	dcMap := map[int]string{}
	dcPool := map[int][]string{}
	for _, dc := range []int{2, 4} {
		dcPool[dc] = append([]string(nil), defaultPool...)
		dcMap[dc] = defaultPool[0]
	}

	for _, item := range dcIPs {
		parts := strings.SplitN(item, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --dc-ip: %s", item)
		}
		dc, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid dc: %s", parts[0])
		}
		if net.ParseIP(parts[1]) == nil {
			return nil, fmt.Errorf("invalid ip: %s", parts[1])
		}
		ip := parts[1]
		dcPool[dc] = []string{ip}
		dcMap[dc] = ip
	}

	for _, item := range dcIPPools {
		parts := strings.SplitN(item, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --dc-ip-pool: %s", item)
		}
		dc, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid dc in --dc-ip-pool: %s", parts[0])
		}
		poolIPs, err := parseIPCSV(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid --dc-ip-pool for dc %d: %w", dc, err)
		}
		dcPool[dc] = append([]string(nil), poolIPs...)
		dcMap[dc] = dcPool[dc][0]
	}

	userDomainProvided := flagProvided("cfproxy-domain")
	userPoolProvided := strings.TrimSpace(*cfproxyDomains) != ""
	userDomain := normalizeCFProxyDomain(*cfproxyDomain)
	userFixedDomain := userDomainProvided && userDomain != ""

	domainPool := defaultCFProxyDomains()
	if userPoolProvided {
		if userDomainProvided {
			return nil, errors.New("use only one of --cfproxy-domain or --cfproxy-domains")
		}
		parsedDomains, err := parseCFProxyDomainCSV(*cfproxyDomains)
		if err != nil {
			return nil, fmt.Errorf("invalid --cfproxy-domains: %w", err)
		}
		domainPool = parsedDomains
	}

	if userDomainProvided {
		if strings.Contains(userDomain, ",") {
			return nil, errors.New("invalid --cfproxy-domain: multiple domains are not allowed; use --cfproxy-domains for comma-separated pool")
		}
		if userDomain == "" {
			return nil, errors.New("invalid --cfproxy-domain: empty domain")
		}
		domainPool = []string{userDomain}
	} else if !userPoolProvided && userDomain != "" {
		domainPool = appendUniqueDomains(domainPool, userDomain)
	}

	normalizedFakeTLSDomain := normalizeCFProxyDomain(*fakeTLSDomain)
	if normalizedFakeTLSDomain != "" && !isLikelyDomain(normalizedFakeTLSDomain) {
		return nil, fmt.Errorf("invalid --fake-tls-domain: %s", *fakeTLSDomain)
	}

	cfg := &Config{
		Host:        *host,
		Port:        *port,
		SecretHex:   *secret,
		GenSecret:   *genSecret,
		FakeTLSDomain: normalizedFakeTLSDomain,
		DCMap:       dcMap,
		DCPool:      dcPool,
		FallbackCFProxy:         !*noCfproxy,
		FallbackCFProxyPriority: *cfproxyPriority,
		FallbackCFProxyDomain:   "",
		FallbackCFProxyUserDomain: userFixedDomain || userPoolProvided,
		FallbackCFProxyRefresh:    !*noCfproxyDomainRefresh,
		FallbackCFProxyDomainsURL: strings.TrimSpace(*cfproxyDomainsURL),
		FallbackCFProxyDomains:    nil,
		FallbackCFProxyActive:     "",
		FallbackCFProxyPerDCActive: make(map[int]string),
		Verbose:     *verbose,
		BufKB:       maxInt(*bufKB, 4),
		PoolSize:    maxInt(*poolSize, 0),
		MaxConns:    maxInt(*maxConns, 1),
		LogFile:     *logFile,
		LogMaxMB:    *logMaxMB,
		LogBackups:  maxInt(*logBackups, 0),
		PprofListen: strings.TrimSpace(*pprofListen),
	}

	cfg.setCFProxyDomains(domainPool)
	return cfg, nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parseIPCSV(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		ip := strings.TrimSpace(p)
		if ip == "" {
			continue
		}
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("invalid ip: %s", ip)
		}
		out = appendUniqueIP(out, ip)
	}
	if len(out) == 0 {
		return nil, errors.New("empty ip pool")
	}
	return out, nil
}

func appendUniqueIP(dst []string, ip string) []string {
	for _, v := range dst {
		if v == ip {
			return dst
		}
	}
	return append(dst, ip)
}

func flagProvided(name string) bool {
	key := "--" + name
	for _, arg := range os.Args[1:] {
		if arg == key || strings.HasPrefix(arg, key+"=") {
			return true
		}
	}
	return false
}
