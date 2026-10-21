package main

import "testing"

func TestFDBudget(t *testing.T) {
	cfg := &Config{MaxConns: 256, PoolSize: 4, DCPool: map[int][]string{2: {"a"}, 4: {"b"}}}
	if fitsFDBudget(cfg, 256) {
		t.Fatal("256 descriptors cannot serve 256 sessions")
	}
	if !fitsFDBudget(cfg, 1024) {
		t.Fatal("standard configuration should fit 1024 descriptors")
	}
	if fitsFDBudget(cfg, 0) {
		t.Fatal("empty budget accepted")
	}
	cfg.PoolSize = int(^uint(0) >> 1)
	if fitsFDBudget(cfg, 1024) {
		t.Fatal("oversized pool accepted")
	}
}
