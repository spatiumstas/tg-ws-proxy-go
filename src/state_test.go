package main

import (
	"testing"
)

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
