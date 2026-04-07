package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
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
	cfproxyDomain := flag.String("cfproxy-domain", defaultCFProxyDomain, "Cloudflare-proxied domain for WS fallback")
	noCfproxy := flag.Bool("no-cfproxy", false, "Disable Cloudflare proxy fallback")
	cfproxyPriority := flag.Bool("cfproxy-priority", true, "Try cfproxy before TCP fallback")
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

	return &Config{
		Host:        *host,
		Port:        *port,
		SecretHex:   *secret,
		GenSecret:   *genSecret,
		DCMap:       dcMap,
		DCPool:      dcPool,
		FallbackCFProxy:         !*noCfproxy,
		FallbackCFProxyPriority: *cfproxyPriority,
		FallbackCFProxyDomain:   strings.TrimSpace(*cfproxyDomain),
		Verbose:     *verbose,
		BufKB:       maxInt(*bufKB, 4),
		PoolSize:    maxInt(*poolSize, 0),
		MaxConns:    maxInt(*maxConns, 1),
		LogFile:     *logFile,
		LogMaxMB:    *logMaxMB,
		LogBackups:  maxInt(*logBackups, 0),
		PprofListen: strings.TrimSpace(*pprofListen),
	}, nil
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
