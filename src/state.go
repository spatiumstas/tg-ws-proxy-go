package main

import (
	"sort"
	"sync"
	"time"
)

var (
	stats         Stats
	pool          = newWSPool()
	frontingUntil time.Time
	frontingMu    sync.Mutex
)

func setFrontingActive() {
	frontingMu.Lock()
	frontingUntil = time.Now().Add(frontingCooldown)
	frontingMu.Unlock()
}

func clearFrontingActive() {
	frontingMu.Lock()
	frontingUntil = time.Time{}
	frontingMu.Unlock()
}

func frontingActive() bool {
	frontingMu.Lock()
	active := time.Now().Before(frontingUntil)
	frontingMu.Unlock()
	return active
}

func sortedDCMap(m map[int]string) []dcMapItem {
	items := make([]dcMapItem, 0, len(m))
	for dc, ip := range m {
		items = append(items, dcMapItem{dc: dc, ip: ip})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].dc < items[j].dc
	})
	return items
}
