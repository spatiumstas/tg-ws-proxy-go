package main

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFakeTLSReplayCache(t *testing.T) {
	var cache fakeTLSReplayCache
	now := time.Now()
	var key [tlsClientRandomLen]byte
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cache.firstSeen(key, now) {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d copies of one hello", accepted.Load())
	}
	if cache.firstSeen(key, now.Add(240*time.Second)) {
		t.Fatal("replay accepted at the end of the timestamp window")
	}
	if !cache.firstSeen(key, now.Add(241*time.Second)) {
		t.Fatal("expired random was not removed")
	}

	for i := 1; i < fakeTLSReplayCacheSize; i++ {
		var other [tlsClientRandomLen]byte
		binary.LittleEndian.PutUint32(other[:], uint32(i))
		cache.firstSeen(other, now.Add(241*time.Second))
	}
	if cache.firstSeen(key, now.Add(241*time.Second)) {
		t.Fatal("a full cache evicted the oldest entry before checking for replay")
	}
	var other [tlsClientRandomLen]byte
	other[31] = 1
	cache.firstSeen(other, now.Add(241*time.Second))
	if len(cache.entries) != fakeTLSReplayCacheSize {
		t.Fatalf("cache grew to %d entries", len(cache.entries))
	}
}
