package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUpstreamTLSVerification(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	for _, tc := range []struct {
		name, host                         string
		fronting, trusted, expired, wantOK bool
	}{
		{"direct", "upstream.test", false, true, false, true},
		{"wrong name", "other.test", false, true, false, false},
		{"unknown CA", "upstream.test", false, false, false, false},
		{"expired", "upstream.test", false, true, true, false},
		{"fronting", "sprinthost.ru", true, true, false, true},
		{"fronting unknown CA", "sprinthost.ru", true, false, false, false},
		{"fronting wrong name", "sprinthost.ru", true, true, false, false},
		{"fronting expired", "sprinthost.ru", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"upstream.test"},
				KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(time.Hour)}
			if tc.name == "fronting wrong name" {
				leaf.DNSNames = []string{"attacker.test"}
			}
			if tc.expired {
				leaf.NotAfter = time.Now().Add(-time.Hour)
			}
			der, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der, rootDER}, PrivateKey: key}}})
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				c, err := ln.Accept()
				if err == nil {
					defer c.Close()
					_ = c.SetDeadline(time.Now().Add(time.Second))
					_ = c.(*tls.Conn).Handshake()
				}
			}()
			verifyName := tc.host
			if tc.fronting {
				verifyName = "upstream.test"
			}
			cfg := upstreamTLSConfig(tc.host, verifyName)
			cfg.RootCAs = x509.NewCertPool()
			if tc.trusted {
				cfg.RootCAs = roots
			}
			c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", ln.Addr().String(), cfg)
			if c != nil {
				_ = c.Close()
			}
			if (err == nil) != tc.wantOK {
				t.Errorf("TLS error = %v, want success %t", err, tc.wantOK)
			}
			<-done
		})
	}
}

func TestUpstreamTLSSessionResumption(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	server.TLS = &tls.Config{MaxVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	cfg := upstreamTLSConfig("127.0.0.1", "127.0.0.1")
	cfg.RootCAs = roots
	cfg.MaxVersion = tls.VersionTLS12
	for i := 0; i < 2; i++ {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", server.Listener.Addr().String(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		resumed := conn.ConnectionState().DidResume
		conn.Close()
		if resumed != (i == 1) {
			t.Fatalf("connection %d resumed=%t", i, resumed)
		}
	}
}
