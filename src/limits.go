package main

import (
	"os"
	"strconv"
	"strings"
)

func checkFDLimit(cfg *Config) {
	data, err := os.ReadFile("/proc/self/limits")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.Join(fields[:3], " ") != "Max open files" {
			continue
		}
		limit, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return
		}
		if !fitsFDBudget(cfg, limit) {
			warnf("max-conns=%d and pools may exceed file descriptor limit %d; reduce max-conns/pool-size or raise ulimit -n", cfg.MaxConns, limit)
		}
		return
	}
}

func fitsFDBudget(cfg *Config, limit uint64) bool {
	// Reserve runtime/stdio and transient dials in addition to steady sockets.
	if limit < 33 {
		return false
	}
	remaining := limit - 33
	for _, ips := range cfg.DCPool {
		for range ips {
			if uint64(cfg.PoolSize) > remaining/4 {
				return false
			}
			remaining -= uint64(cfg.PoolSize) * 4
		}
	}
	if cfg.PoolSize > 0 && cfg.hasCFProxyWorkerDomains() {
		if remaining < 12 {
			return false
		}
		remaining -= 12
	}
	return uint64(cfg.MaxConns) <= remaining/2
}
