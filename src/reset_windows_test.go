package main

import (
	"fmt"
	"syscall"
	"testing"
)

func TestIsConnectionResetWindows(t *testing.T) {
	if !isConnectionReset(fmt.Errorf("read: %w", syscall.WSAECONNRESET)) {
		t.Fatal("WSAECONNRESET must be recognized")
	}
}
