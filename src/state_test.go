package main

import (
	"testing"
	"time"
)

func TestCooldown(t *testing.T) {
	k := dcKey{DC: 1, IsMedia: false}
	clearCooldown(k)
	if inCooldown(k) {
		t.Fatal("not in cooldown after clear")
	}
	setCooldown(k)
	if !inCooldown(k) {
		t.Fatal("expected in cooldown after set")
	}
	clearCooldown(k)
	if inCooldown(k) {
		t.Fatal("expected not in cooldown after clear")
	}
}

func TestBlacklistTTL(t *testing.T) {
	k := dcKey{DC: 2, IsMedia: true}
	setBlacklisted(k)
	if !isBlacklisted(2, true) {
		t.Fatal("expected blacklisted right after set")
	}
	if isBlacklisted(3, false) {
		t.Fatal("unrelated key must not be blacklisted")
	}

	// Force expiry by rewinding the stored deadline into the past.
	blMu.Lock()
	blacklist[k] = time.Now().Add(-time.Minute)
	blMu.Unlock()

	if isBlacklisted(2, true) {
		t.Fatal("expected expired blacklist entry to report false")
	}
	// Expired entry must be lazily removed.
	blMu.Lock()
	_, ok := blacklist[k]
	blMu.Unlock()
	if ok {
		t.Fatal("expired blacklist entry should be deleted")
	}
}

func TestIPCooldown(t *testing.T) {
	ip := "149.154.167.51"
	clearIPCooldown(ip)
	if inIPCooldown(ip) {
		t.Fatal("not in IP cooldown after clear")
	}
	setIPCooldown(ip)
	if !inIPCooldown(ip) {
		t.Fatal("expected in IP cooldown after set")
	}
	clearIPCooldown(ip)
	if inIPCooldown(ip) {
		t.Fatal("expected not in IP cooldown after clear")
	}
}

func TestIPCooldownTTL(t *testing.T) {
	ip := "149.154.175.50"
	setIPCooldown(ip)
	if !inIPCooldown(ip) {
		t.Fatal("expected in IP cooldown right after set")
	}

	ipFuMu.Lock()
	ipFailUntil[ip] = time.Now().Add(-time.Minute)
	ipFuMu.Unlock()

	if inIPCooldown(ip) {
		t.Fatal("expected expired IP cooldown entry to report false")
	}
	ipFuMu.Lock()
	_, ok := ipFailUntil[ip]
	ipFuMu.Unlock()
	if ok {
		t.Fatal("expired IP cooldown entry should be deleted")
	}
}

func TestEmptyIPCooldownNoop(t *testing.T) {
	setIPCooldown("")
	if inIPCooldown("") {
		t.Fatal("empty IP must never be in cooldown")
	}
	clearIPCooldown("")
}

func TestFrontingActive(t *testing.T) {
	clearFrontingActive()
	if frontingActive() {
		t.Fatal("fronting should be inactive after clear")
	}
	setFrontingActive()
	if !frontingActive() {
		t.Fatal("fronting should be active after set")
	}
	clearFrontingActive()
	if frontingActive() {
		t.Fatal("fronting should be inactive after second clear")
	}
}

func TestSplitWSTargetsKeepsCooldownIPForFronting(t *testing.T) {
	const cooldownIP = "149.154.167.220"
	clearIPCooldown(cooldownIP)
	defer clearIPCooldown(cooldownIP)
	setIPCooldown(cooldownIP)

	directTargets, frontingTargets := splitWSTargets([]string{cooldownIP}, true)
	if len(directTargets) != 0 {
		t.Fatalf("direct targets = %v, want none during cooldown", directTargets)
	}
	if len(frontingTargets) != 1 || frontingTargets[0] != cooldownIP {
		t.Fatalf("fronting targets = %v, want [%s]", frontingTargets, cooldownIP)
	}
}
