// Package pki implements the minimal internal Certificate Authority the
// control plane uses to issue mTLS client certificates to agents
// (ARCHITECTURE.md §G — "Certificate rotation strategy", §8 — agent security
// requirement). Deliberately stdlib-only (crypto/x509): no external PKI
// dependency for something this self-contained.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// CA holds the internal root CA's key material. In production this key
// must come from the secrets store (ARCHITECTURE.md §G), never from a
// plaintext file — the constructor here accepts key bytes so the caller
// controls sourcing.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// GenerateCA creates a fresh, self-signed root CA. Intended for first-run
// bootstrap or dev/test use; the resulting key material should be persisted
// to the secrets store by the caller so it survives a restart (losing the
// CA key invalidates every issued agent certificate).
func GenerateCA(commonName string, validFor time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(validFor),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	return &CA{cert: cert, key: key}, nil
}

// CertPEM returns the CA's own certificate in PEM form — this is what
// agents must trust (pinned at enrollment time) to validate the control
// plane's server certificate, and what the server uses as its client-cert
// verification root.
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

// KeyPEM returns the CA's private key in PEM form. This is the sensitive
// half of the CA's material — in production it must go to a real secrets
// store (ARCHITECTURE.md §G), never a plaintext file on the same disk as
// everything else. LoadCA is the file-based persistence this build
// actually uses (see cmd/controlplane), which is a known, documented
// simplification rather than the eventual secrets-store integration.
func (c *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// LoadCA reconstructs a CA from previously persisted cert/key PEM (as
// produced by CertPEM/KeyPEM), so a restarted control plane can keep
// issuing certificates under the same root rather than invalidating every
// already-enrolled agent. It verifies the key actually matches the
// certificate's public key before returning, so a mismatched or corrupted
// pair fails loudly at startup instead of producing a CA that can't
// actually sign anything verifiable.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("invalid CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}

	certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !certPub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("CA key does not match CA certificate")
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("loaded certificate is not a CA certificate")
	}

	return &CA{cert: cert, key: key}, nil
}

func (c *CA) Certificate() *x509.Certificate { return c.cert }

// IssueAgentCert signs a CSR for a specific host/agent identity. The
// certificate's CommonName is set to hostID so the mTLS server can recover
// the caller's identity directly from the verified peer certificate — no
// extra registry lookup needed on every request (ARCHITECTURE.md §G).
func (c *CA) IssueAgentCert(csrDER []byte, hostID string, validFor time.Duration) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("invalid CSR signature: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostID},
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     time.Now().Add(validFor),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("sign agent certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// ServerTLSCertificate generates a certificate for the control plane's own
// mTLS listener, signed by this CA, so agents that trust the CA (pinned at
// enrollment) also trust this server identity.
//
// hosts may mix DNS names ("localhost", "controlplane.internal") and IP
// literals ("127.0.0.1", "10.0.0.5") — each is routed to the correct SAN
// list. This matters because Go's TLS client only matches an IP-literal
// dial address against the certificate's IPAddresses field, never against
// DNSNames: putting "127.0.0.1" in DNSNames produces a certificate that
// looks fine on inspection but fails verification for every client that
// actually connects to that IP, which is exactly how agents dial the
// control plane by address in this build (ARCHITECTURE.md §G).
func (c *CA) ServerTLSCertificate(hosts []string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	var dnsNames []string
	var ipAddresses []net.IP
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ipAddresses = append(ipAddresses, ip)
		} else {
			dnsNames = append(dnsNames, h)
		}
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "docker-monitor-control-plane"},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddresses,
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     time.Now().Add(validFor),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign server certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal server key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
