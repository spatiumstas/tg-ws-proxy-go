package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
)

func upstreamTLSConfig(serverName string, fronting bool) *tls.Config {
	cfg := &tls.Config{ServerName: serverName}
	if fronting {
		// Fronting changes SNI; verify trust without requiring the certificate to match it.
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
				Roots: cfg.RootCAs, Intermediates: intermediates,
			})
			return err
		}
	}
	return cfg
}
