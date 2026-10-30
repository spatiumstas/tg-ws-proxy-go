package main

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestFakeTLSProfileRecordsAndTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		flight       []byte
		continuous   bool
		blockedWrite bool
		want         int
	}{
		{"fragmented first flight", []byte{0x16, 3, 3, 0, 1, 0, 0x14, 3, 3, 0, 1, 1, 0x17, 3, 3, 0, 27}, false, false, 27},
		{"alert", []byte{0x15, 3, 3, 0, 2}, false, false, 0},
		{"garbage", []byte("HTTP/"), false, false, 0},
		{"empty appdata", []byte{0x17, 3, 3, 0, 0}, false, false, 0},
		{"oversized record", []byte{0x16, 3, 3, 0xff, 0xff}, false, false, 0},
		{"partial header", []byte{0x16, 3}, false, false, 0},
		{"partial body", []byte{0x16, 3, 3, 0, 2, 0}, false, false, 0},
		{"blocked read", nil, false, false, 0},
		{"continuous CCS", nil, true, false, 0},
		{"blocked write", nil, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			client, server := net.Pipe()
			done := make(chan struct{})
			t.Cleanup(func() {
				_ = client.Close()
				_ = server.Close()
				<-done
			})
			go func() {
				defer close(done)
				defer server.Close()
				if tc.blockedWrite {
					<-ctx.Done()
					return
				}
				hello, err := readFakeTLSRecordForTest(server)
				if err != nil {
					t.Errorf("read probe ClientHello: %v", err)
					return
				}
				info := parseFakeTLSClientHello(hello)
				if !reflect.DeepEqual(info.keyShares, map[uint16]int{tlsGroupX25519: 32}) {
					t.Errorf("probe must offer only X25519: %+v", info.keyShares)
				}
				if tc.continuous {
					for {
						if _, err := server.Write(fakeTLSCCSFrame); err != nil {
							return
						}
					}
				}
				for _, b := range tc.flight {
					if _, err := server.Write([]byte{b}); err != nil {
						return
					}
				}
				if tc.flight == nil {
					<-ctx.Done()
				}
			}()
			started := time.Now()
			length, err := probeFakeTLSConn(ctx, client, "mask.example")
			if length != tc.want || (err == nil) != (tc.want != 0) {
				t.Fatalf("profile = %d, %v; want %d", length, err, tc.want)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("probe exceeded shared deadline: %v", elapsed)
			}
		})
	}
}

func TestFakeTLSProfileRealTLS13Server(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	helloChecked := make(chan bool, 1)
	server.TLS = &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			helloChecked <- hello.ServerName == "mask.example" &&
				reflect.DeepEqual(hello.SupportedCurves, []tls.CurveID{tls.X25519}) &&
				reflect.DeepEqual(hello.SupportedVersions, []uint16{tls.VersionTLS13}) &&
				reflect.DeepEqual(hello.SupportedProtos, []string{"h2", "http/1.1"})
			return nil, nil
		},
	}
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	length, err := probeFakeTLSAppDataLen(ctx, server.Listener.Addr().String(), "mask.example")
	if err != nil || length <= 0 {
		t.Fatalf("profile of TLS 1.3 server = %d, %v", length, err)
	}
	if !<-helloChecked {
		t.Fatal("probe ClientHello has incorrect SNI, groups, versions or ALPN")
	}
}

func TestFakeTLSProfileUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if length, err := probeFakeTLSAppDataLen(ctx, address, "mask.example"); length != 0 || err == nil {
		t.Fatalf("unreachable site returned %d, %v", length, err)
	}
}
