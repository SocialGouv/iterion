package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
)

func irTestDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// One publication's compiled IR is named by its run, its bytes and a nonce
// drawn for that publication: "<run>-bundle-ir-<digest>-<32 lowercase hex>",
// inside the run's "-bundle-" family under its own "ir-" infix. Two
// publications never share an id — not even two of identical bytes, so a
// later one never replaces the bytes an earlier message points at.
func TestIRContentLogicalIDNamesTheRunTheBytesAndThePublication(t *testing.T) {
	digest := irTestDigest("program a")
	shape := regexp.MustCompile(`^run-1-bundle-ir-` + digest + `-[0-9a-f]{32}$`)
	a, err := IRContentLogicalID("run-1", digest, rand.Reader)
	if err != nil {
		t.Fatalf("IRContentLogicalID: %v", err)
	}
	if !shape.MatchString(a) {
		t.Fatalf("id = %q, want run-1-bundle-ir-%s-<32 lowercase hex>", a, digest)
	}
	again, err := IRContentLogicalID("run-1", digest, rand.Reader)
	if err != nil {
		t.Fatalf("IRContentLogicalID: %v", err)
	}
	if again == a {
		t.Fatalf("two publications of the same bytes share the id %q: the later one would replace the earlier one's object", a)
	}
	other, err := IRContentLogicalID("run-1", irTestDigest("program b"), rand.Reader)
	if err != nil {
		t.Fatalf("IRContentLogicalID: %v", err)
	}
	if strings.Contains(other, digest) {
		t.Fatalf("program b's id %q names program a's digest", other)
	}
	for _, tc := range []struct{ name, runID, digest string }{
		{"empty digest", "run-1", ""},
		{"uppercase digest", "run-1", strings.ToUpper(irTestDigest("x"))},
		{"digest with a separator", "run-1", "../" + irTestDigest("x")[3:]},
		{"run id with a separator", "run/1", irTestDigest("x")},
		{"empty run id", "", irTestDigest("x")},
	} {
		if id, err := IRContentLogicalID(tc.runID, tc.digest, rand.Reader); err == nil {
			t.Errorf("%s: IRContentLogicalID(%q, %q) = %q, want an error", tc.name, tc.runID, tc.digest, id)
		}
	}
}

// The nonce is the part of the id no caller can predict: a source that
// fails, or that runs dry before 128 bits, refuses the id with its cause
// instead of naming the blob with a predictable one.
func TestIRContentLogicalIDRefusesANonceSourceThatFails(t *testing.T) {
	cause := errors.New("entropy source unavailable")
	for _, tc := range []struct {
		name   string
		source io.Reader
		want   error
	}{
		{"failing source", iotest.ErrReader(cause), cause},
		{"source short of 128 bits", bytes.NewReader(make([]byte, 15)), io.ErrUnexpectedEOF},
		{"empty source", bytes.NewReader(nil), io.EOF},
	} {
		id, err := IRContentLogicalID("run-1", irTestDigest("program"), tc.source)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: IRContentLogicalID = (%q, %v), want a refusal wrapping %v", tc.name, id, err, tc.want)
		}
		if id != "" {
			t.Fatalf("%s: a refused id still came back: %q", tc.name, id)
		}
	}
}

// Every id of a run's IR family carries the infix the reservation refuses,
// an id ending with "-bundle" is refused too — the sweep prefix of run "x",
// ir/x-bundle-, holds the whole family of run "x-bundle" — and no id the
// platform mints takes either shape.
func TestRunIDCarriesIRBlobFamilyInfix(t *testing.T) {
	const owner = "019ac3d2-7f1e-7c4b-9a0e-2b5c8d1e4f60"
	content, err := IRContentLogicalID(owner, irTestDigest("program"), rand.Reader)
	if err != nil {
		t.Fatalf("IRContentLogicalID: %v", err)
	}
	for _, id := range []string{content, owner + "-bundle-" + irTestDigest("program"), owner + "-bundle", "x-bundle", "-bundle"} {
		if !RunIDCarriesIRBlobFamilyInfix(id) {
			t.Errorf("run id %q shares an IR blob key or a sweep prefix with another run, and the reservation lets it through", id)
		}
	}
	minted, err := GenerateRunID()
	if err != nil {
		t.Fatalf("GenerateRunID: %v", err)
	}
	for _, id := range []string{minted, "idem-6a1d3c55-1f0e-5b7a-9c2e-4d8f0b3a7e21", "repair-0123456789abcdef01234567-1", "run-bundled", "bundle-run", "bundle", "x-bundles", "xbundle"} {
		if RunIDCarriesIRBlobFamilyInfix(id) {
			t.Errorf("run id %q takes neither reserved shape, and the reservation refuses it", id)
		}
	}
}
