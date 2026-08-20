package netproxy

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// JavaTrustStorePassword is the well-known password of the PKCS12
// truststore built for JVM clients. Deliberately not a secret: the
// store holds only public CA certificates, and "changeit" is the JDK's
// own cacerts convention, so every keytool/-Djavax example works
// unchanged.
const JavaTrustStorePassword = "changeit"

// systemRootsBundle is the PEM bundle shipped by Debian/Alpine images
// (the official iterion image included). Folding it into the JVM
// truststore keeps non-inspected TLS (NO_PROXY'd hosts, in-cluster
// endpoints) validating exactly as before — a truststore holding ONLY
// the egress CA would silently break those.
const systemRootsBundle = "/etc/ssl/certs/ca-certificates.crt"

// JavaTrustStorePKCS12 renders a PKCS12 truststore for JVM clients:
// the egress-inspection CA plus the host's system roots. The CA env
// vars the sandbox already sets (SSL_CERT_FILE, CURL_CA_BUNDLE,
// NODE_EXTRA_CA_CERTS, …) are honoured by OpenSSL, curl, git, Node,
// python and nix — but never by a JVM, whose trust is a keystore-typed
// file: without this store, the first `gradlew build` behind the
// inspecting proxy dies in SunCertPathBuilder and every agent
// rediscovers keytool from scratch.
//
// Returns the DER store and how many system roots were folded in.
// A missing/unreadable system bundle degrades to an egress-CA-only
// store (still correct for every inspected connection — the proxy
// re-signs ALL of them) and reports 0 so the caller can log the
// narrower coverage instead of hiding it. The store is encoded with
// the legacy PKCS12 ciphers on purpose: JDK 8 keystores predate PBES2
// support, and the payload is public certificates — there is nothing
// to protect.
func JavaTrustStorePKCS12(caPEM []byte) ([]byte, int, error) {
	certs, err := parsePEMCertificates(caPEM)
	if err != nil {
		return nil, 0, fmt.Errorf("netproxy: java truststore: parse egress CA: %w", err)
	}
	if len(certs) == 0 {
		return nil, 0, fmt.Errorf("netproxy: java truststore: egress CA PEM holds no certificate")
	}
	systemRoots := 0
	if raw, rerr := os.ReadFile(systemRootsBundle); rerr == nil {
		if roots, perr := parsePEMCertificates(raw); perr == nil {
			certs = append(certs, roots...)
			systemRoots = len(roots)
		}
	}
	der, err := pkcs12.Legacy.EncodeTrustStore(certs, JavaTrustStorePassword)
	if err != nil {
		return nil, 0, fmt.Errorf("netproxy: java truststore: encode PKCS12: %w", err)
	}
	return der, systemRoots, nil
}

// parsePEMCertificates parses every CERTIFICATE block in a PEM bundle,
// skipping non-certificate blocks (a system bundle may carry text
// headers between blocks). A block that claims to be a certificate but
// does not parse is an error, never skipped silently.
func parsePEMCertificates(bundle []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate block: %w", err)
		}
		certs = append(certs, cert)
	}
	return certs, nil
}
