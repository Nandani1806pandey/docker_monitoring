package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"testing"
	"time"
)

func TestGenerateCA(t *testing.T) {
	ca, err := GenerateCA("test-root", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if !ca.Certificate().IsCA {
		t.Fatalf("expected IsCA=true on generated root")
	}
	if len(ca.CertPEM()) == 0 {
		t.Fatalf("expected non-empty CertPEM")
	}
}

func TestIssueAgentCert_CommonNameIsHostID(t *testing.T) {
	ca, err := GenerateCA("test-root", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	agentKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	// Even if the CSR asks for a different CommonName, IssueAgentCert must
	// ignore it — the caller's hostID argument is the only source of truth
	// (ARCHITECTURE.md §G: an agent cannot request an identity other than
	// the one it was enrolled for).
	csrTmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "attacker-chosen-name"}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, agentKey)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}

	certPEM, err := ca.IssueAgentCert(csrDER, "host-abc", time.Hour)
	if err != nil {
		t.Fatalf("IssueAgentCert: %v", err)
	}
	leaf := parseCertPEM(t, certPEM)
	if leaf.Subject.CommonName != "host-abc" {
		t.Fatalf("expected CommonName forced to hostID %q, got %q", "host-abc", leaf.Subject.CommonName)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("issued cert does not verify against CA: %v", err)
	}
}

func TestIssueAgentCert_RejectsMalformedCSR(t *testing.T) {
	ca, err := GenerateCA("test-root", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if _, err := ca.IssueAgentCert([]byte("not a csr"), "host-abc", time.Hour); err == nil {
		t.Fatalf("expected error for garbage CSR bytes")
	}
}

func TestIssueAgentCert_RejectsTamperedSignature(t *testing.T) {
	ca, err := GenerateCA("test-root", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	agentKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrTmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "host-abc"}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, agentKey)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	// Flip a byte inside the signature so CheckSignature (called by
	// IssueAgentCert before any certificate is issued) must reject it.
	tampered := append([]byte(nil), csrDER...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := ca.IssueAgentCert(tampered, "host-abc", time.Hour); err == nil {
		t.Fatalf("expected error for CSR with tampered signature")
	}
}

// TestServerTLSCertificate_IPLiteralHandshake is a regression test for a
// real bug: ServerTLSCertificate was putting IP literals like "127.0.0.1"
// into DNSNames instead of IPAddresses. Go's TLS client only matches an
// IP-literal dial address against IPAddresses, never DNSNames, so every
// localhost/IP-based mTLS connection failed verification even though the
// certificate "looked" fine under field inspection. This test does a real
// handshake against 127.0.0.1 rather than just inspecting SAN fields, so
// it can't pass for the wrong reason the way a field-only test could.
func TestServerTLSCertificate_IPLiteralHandshake(t *testing.T) {
	ca, err := GenerateCA("test-root", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	certPEM, keyPEM, err := ca.ServerTLSCertificate([]string{"127.0.0.1", "localhost"}, time.Hour)
	if err != nil {
		t.Fatalf("ServerTLSCertificate: %v", err)
	}

	leaf := parseCertPEM(t, certPEM)
	if len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("expected 127.0.0.1 routed into IPAddresses, got %v", leaf.IPAddresses)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" {
		t.Fatalf("expected localhost routed into DNSNames, got %v", leaf.DNSNames)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.(*tls.Conn).Handshake()
	}()

	// Dial the listener's actual 127.0.0.1 address — this is what exercises
	// Go's SAN-matching logic against an IP literal, not a hostname.
	clientConn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: pool})
	if err != nil {
		t.Fatalf("TLS handshake against 127.0.0.1 failed (SAN regression): %v", err)
	}
	defer clientConn.Close()
}

func parseCertPEM(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("failed to PEM-decode certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

// TestCA_PersistAndReload is a regression test for restart-survivability:
// a CA saved via CertPEM/KeyPEM and reloaded via LoadCA must be able to
// issue certificates that verify against the ORIGINAL CertPEM — proving
// the reload actually reconstructed usable key material, not just parsed
// bytes without error.
func TestCA_PersistAndReload(t *testing.T) {
	original, err := GenerateCA("test-root", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	keyPEM, err := original.KeyPEM()
	if err != nil {
		t.Fatalf("KeyPEM: %v", err)
	}
	certPEM := original.CertPEM()

	reloaded, err := LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if reloaded.Certificate().SerialNumber.Cmp(original.Certificate().SerialNumber) != 0 {
		t.Fatalf("reloaded CA has a different serial than the original")
	}

	// Issue a cert with the RELOADED ca and verify it against a pool built
	// from the ORIGINAL's CertPEM output — this only passes if LoadCA
	// reconstructed key material that actually corresponds to that cert.
	agentKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: "host-after-restart"}}, agentKey)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	issuedPEM, err := reloaded.IssueAgentCert(csrDER, "host-after-restart", time.Hour)
	if err != nil {
		t.Fatalf("IssueAgentCert (reloaded CA): %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatalf("failed to build pool from original CertPEM")
	}
	leaf := parseCertPEM(t, issuedPEM)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("cert issued by reloaded CA does not verify against original CA cert: %v", err)
	}
}

func TestLoadCA_RejectsMismatchedKey(t *testing.T) {
	ca1, err := GenerateCA("root-1", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	ca2, err := GenerateCA("root-2", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	key2PEM, err := ca2.KeyPEM()
	if err != nil {
		t.Fatalf("KeyPEM: %v", err)
	}
	// ca1's certificate paired with ca2's key: the public keys don't match.
	if _, err := LoadCA(ca1.CertPEM(), key2PEM); err == nil {
		t.Fatalf("expected LoadCA to reject a cert/key pair whose public keys don't match")
	}
}

func TestLoadCA_RejectsGarbagePEM(t *testing.T) {
	if _, err := LoadCA([]byte("not pem"), []byte("also not pem")); err == nil {
		t.Fatalf("expected LoadCA to reject garbage PEM input")
	}
}
