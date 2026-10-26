package boottls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

func Config(credentials []byte, server bool) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(credentials, credentials)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(credentials) {
		return nil, fmt.Errorf("invalid guest certificate")
	}
	c := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: roots, ServerName: "care-guest"}
	if server {
		c.ClientCAs = roots
		c.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return c, nil
}
