package netproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"
)

// EphemeralCA is a per-run certificate authority used by the proxy's
// TLS-inspection mode (Layer 2 secret egress substitution). It is
// generated fresh per run, lives only in memory, and is never
// persisted — its sole job is to mint short-lived leaf certificates for
// the hosts a sandboxed agent connects to, so the proxy can terminate
// TLS, rewrite the plaintext request (placeholder→secret, DLP), and
// re-encrypt to the real upstream.
//
// The CA's public certificate (CertPEM) is injected into the sandbox's
// trust stores (system + NODE_EXTRA_CA_CERTS) so in-container clients
// accept the minted leaves. The private key NEVER leaves the host
// process.
type EphemeralCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate // host → cached leaf
}

// NewEphemeralCA generates a fresh in-memory CA.
func NewEphemeralCA() (*EphemeralCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("netproxy: generate CA key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "iterion sandbox egress CA",
			Organization: []string{"iterion"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("netproxy: create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("netproxy: parse CA cert: %w", err)
	}
	return &EphemeralCA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves:  make(map[string]*tls.Certificate),
	}, nil
}

// CertPEM returns the PEM-encoded CA certificate to inject into the
// sandbox trust stores.
func (ca *EphemeralCA) CertPEM() []byte { return ca.certPEM }

// GetCertificate is the tls.Config.GetCertificate callback: it mints (or
// returns a cached) leaf for the SNI host the client requested.
func (ca *EphemeralCA) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	if host == "" {
		// No SNI (rare): fall back to a generic leaf. The connection
		// will still verify against the CA on the client side.
		host = "localhost"
	}
	return ca.leafFor(host)
}

// leafFor mints (and caches) a leaf certificate for host, signed by the
// CA. The leaf carries host as a SAN (DNS or IP as appropriate).
func (ca *EphemeralCA) leafFor(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if leaf, ok := ca.leaves[host]; ok {
		return leaf, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("netproxy: generate leaf key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("netproxy: create leaf cert: %w", err)
	}
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, ca.cert.Raw},
		PrivateKey:  key,
	}
	ca.leaves[host] = leaf
	return leaf, nil
}

// randSerial returns a positive 128-bit random serial number. crypto/rand
// is mixed with a SHA-256 fold so the value is well-distributed even if
// the entropy source is slow.
func randSerial() (*big.Int, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, fmt.Errorf("netproxy: serial entropy: %w", err)
	}
	sum := sha256.Sum256(buf[:])
	// Clear the top bit so the serial stays positive.
	sum[0] &= 0x7f
	return new(big.Int).SetBytes(sum[:16]), nil
}
