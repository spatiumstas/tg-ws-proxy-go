package main

import (
	"fmt"
	"math"
	"net"
)

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

func setSockOpts(c net.Conn, bufSize int) error {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return nil
	}
	_ = tcp.SetNoDelay(true)
	_ = tcp.SetReadBuffer(bufSize)
	_ = tcp.SetWriteBuffer(bufSize)
	return nil
}
