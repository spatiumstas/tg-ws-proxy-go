//go:build !windows

package main

import (
	"errors"
	"syscall"
)

func isConnectionReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}
