package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
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
			opts := x509.VerifyOptions{
				Roots: cfg.RootCAs, Intermediates: intermediates, DNSName: verifyName,
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			var nameErr x509.HostnameError
			if errors.As(err, &nameErr) && serverName == "sprinthost.ru" && strings.HasSuffix(verifyName, ".web.telegram.org") {
				// Telegram's fronted endpoint may return its main-site certificate.
				opts.DNSName = "telegram.org"
				_, err = cs.PeerCertificates[0].Verify(opts)
			}
			return err
		}
	}
	return cfg
}
