// Package tlstest makes throwaway TLS certificates for tests.
package tlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"
)

// CA is a self-signed certificate for 127.0.0.1 and localhost, and a pool that trusts it.
type CA struct {
	Server *tls.Config
	Pool   *x509.CertPool
}

// New makes a fresh CA for one test.
func New(t testing.TB) *CA {
	t.Helper()
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// NewCA makes a fresh CA, for callers without a testing.TB such as TestMain.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "chainmail test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parsing certificate: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{
		Server: &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}},
			MinVersion:   tls.VersionTLS12,
		},
		Pool: pool,
	}, nil
}

// PEM returns the server certificate and its private key as PEM, for a
// server running outside the test process.
func (ca *CA) PEM() (cert, key []byte, err error) {
	c := ca.Server.Certificates[0]
	k, ok := c.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("tlstest: the key is not ECDSA")
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding key: %w", err)
	}
	cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})
	key = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	return cert, key, nil
}
