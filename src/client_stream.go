package main

import (
	"context"
	"net"
)

type clientReadConn struct {
	net.Conn
	pending []byte
	err     error
	done    chan struct{}
	cancel  context.CancelFunc
	stop    func() bool
}

func watchClient(ctx context.Context, conn net.Conn) (context.Context, net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	client := &clientReadConn{Conn: conn, done: make(chan struct{}), cancel: cancel}
	client.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	go func() {
		defer close(client.done)
		// Hand early bytes to the bridge without a second reader or a permanent copy loop.
		// shortcut: EOF after early data is observed by the bridge; use a bounded queue if setup needs more read-ahead.
		buf := make([]byte, 32*1024)
		n, err := conn.Read(buf)
		client.pending, client.err = buf[:n], err
		if n == 0 && err != nil {
			cancel()
		}
	}()
	return ctx, client
}

func (c *clientReadConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	<-c.done
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		if len(c.pending) > 0 {
			return n, nil
		}
		c.pending = nil
		return n, c.err
	}
	if c.err != nil {
		return 0, c.err
	}
	return c.Conn.Read(p)
}

func (c *clientReadConn) Close() error {
	c.stop()
	c.cancel()
	err := c.Conn.Close()
	<-c.done
	return err
}
