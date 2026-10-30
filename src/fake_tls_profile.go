package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

var errFakeTLSProbeHelloSent = errors.New("fake TLS probe ClientHello sent")

type fakeTLSProbeConn struct{ net.Conn }

// Stop crypto/tls after sending ClientHello so the probe can read raw TLS records.
func (fakeTLSProbeConn) Read([]byte) (int, error) {
	return 0, errFakeTLSProbeHelloSent
}

func probeFakeTLSAppDataLen(ctx context.Context, address, serverName string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := newUpstreamDialer(5*time.Second).DialContext(ctx, "tcp", address)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return probeFakeTLSConn(ctx, conn, serverName)
}

func probeFakeTLSConn(ctx context.Context, conn net.Conn, serverName string) (int, error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}
	client := tls.Client(fakeTLSProbeConn{conn}, &tls.Config{
		ServerName:       serverName,
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519},
		NextProtos:       []string{"h2", "http/1.1"},
	})
	if err := client.HandshakeContext(ctx); !errors.Is(err, errFakeTLSProbeHelloSent) {
		return 0, fmt.Errorf("send profile ClientHello: %w", err)
	}
	for {
		var header [5]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return 0, err
		}
		length := int(binary.BigEndian.Uint16(header[3:]))
		if header[1] != 0x03 || header[2] != 0x03 || length == 0 || length > tlsRecordMaxLen+256 {
			return 0, fmt.Errorf("invalid profile TLS record")
		}
		switch header[0] {
		case tlsRecordAppData:
			return length, nil
		case tlsRecordHandshake, tlsRecordCCS:
			if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("unexpected profile TLS record: 0x%02x", header[0])
		}
	}
}
