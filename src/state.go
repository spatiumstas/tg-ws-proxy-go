package main

import (
	"sort"
	"sync"
	"time"
)

var (
	stats     Stats
	pool      = newWSPool()
	blacklist = make(map[dcKey]struct{})
	blMu      sync.Mutex
	failUntil = make(map[dcKey]time.Time)
	fuMu      sync.Mutex
)

func setCooldown(k dcKey) {
	fuMu.Lock()
	failUntil[k] = time.Now().Add(dcFailCooldown)
	fuMu.Unlock()
}

func inCooldown(k dcKey) bool {
	fuMu.Lock()
	t, ok := failUntil[k]
	fuMu.Unlock()
	return ok && time.Now().Before(t)
}

func clearCooldown(k dcKey) {
	fuMu.Lock()
	delete(failUntil, k)
	fuMu.Unlock()
}

func setBlacklisted(k dcKey) {
	blMu.Lock()
	blacklist[k] = struct{}{}
	blMu.Unlock()
}

func isBlacklisted(dc int, media bool) bool {
	blMu.Lock()
	_, ok := blacklist[dcKey{DC: dc, IsMedia: media}]
	blMu.Unlock()
	return ok
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
