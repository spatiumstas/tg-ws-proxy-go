package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
)

var upstreamSessionCache = tls.NewLRUClientSessionCache(256)

func upstreamTLSConfig(serverName string, verifyName string) *tls.Config {
	cfg := &tls.Config{ServerName: serverName, ClientSessionCache: upstreamSessionCache}
	if serverName != verifyName {
		// Verify the real upstream identity while sending the masking SNI.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("upstream sent no certificate")
			}
			intermediates := x509.NewCertPool()
			for _, cert := range cs.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots: cfg.RootCAs, Intermediates: intermediates, DNSName: verifyName,
			})
			return err
		}
	}
	return cfg
}
