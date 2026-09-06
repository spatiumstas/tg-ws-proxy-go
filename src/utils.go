package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"time"
)

var tcpKeepAliveConfig = net.KeepAliveConfig{
	Enable:   true,
	Idle:     10 * time.Second,
	Interval: 5 * time.Second,
	Count:    3,
}

func newUpstreamDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAliveConfig: tcpKeepAliveConfig}
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%.1f", v)
}

func humanBytes(n int64) string {
	v := float64(n)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	u := 0
	for math.Abs(v) >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	return formatFloat(v) + units[u]
}

func getLinkHost(host string) string {
	if host != "0.0.0.0" {
		return host
	}
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer c.Close()
	la, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || la.IP == nil {
		return "127.0.0.1"
	}
	return la.IP.String()
}

func isRedirect(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout() ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded)
}

func isFrontingRetryError(err error) bool {
	return isTimeoutError(err) || isConnectionReset(err)
}

func setSockOpts(c net.Conn, bufSize int) error {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return nil
	}
	_ = tcp.SetNoDelay(true)
	_ = tcp.SetReadBuffer(bufSize)
	_ = tcp.SetWriteBuffer(bufSize)
	_ = tcp.SetKeepAliveConfig(tcpKeepAliveConfig)
	return nil
}
