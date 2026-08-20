package netproxy

import (
	"strings"
	"testing"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// The JVM truststore must round-trip: the egress CA fed in comes back
// out of the decoded store. Falsified both ways — garbage PEM is a
// named refusal, never an empty store.
func TestJavaTrustStoreRoundTrip(t *testing.T) {
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatalf("ephemeral CA: %v", err)
	}
	der, systemRoots, err := JavaTrustStorePKCS12(ca.CertPEM())
	if err != nil {
		t.Fatalf("JavaTrustStorePKCS12: %v", err)
	}
	certs, err := pkcs12.DecodeTrustStore(der, JavaTrustStorePassword)
	if err != nil {
		t.Fatalf("decode truststore: %v", err)
	}
	// systemRoots is environment-dependent (present on the CI image,
	// possibly absent on a dev laptop): assert the RELATION, not a count.
	if want := 1 + systemRoots; len(certs) != want {
		t.Fatalf("store holds %d certs, want %d (egress CA + %d system roots)", len(certs), want, systemRoots)
	}
	found := false
	for _, c := range certs {
		if c.Subject.CommonName == "iterion sandbox egress CA" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("egress CA not present in the decoded truststore")
	}
}

func TestJavaTrustStoreRefusesGarbage(t *testing.T) {
	if _, _, err := JavaTrustStorePKCS12([]byte("not a pem")); err == nil {
		t.Fatal("garbage PEM produced a truststore, want a named refusal")
	} else if !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("error = %q, want it to name the missing certificate", err)
	}
}
