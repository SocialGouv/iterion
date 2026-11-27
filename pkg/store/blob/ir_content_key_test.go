package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func irContentID(t *testing.T, runID string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	id, err := store.IRContentLogicalID(runID, hex.EncodeToString(sum[:]), rand.Reader)
	if err != nil {
		t.Fatalf("IRContentLogicalID: %v", err)
	}
	return id
}

// A compiled IR's content key is a key the release before it already
// serves: its storage-key validation (validateIRBlobKey, IRBlobKeyRunID)
// accepts it and hands back the logical id, and it sits under the run's
// "-bundle-" prefix — the prefix DeleteRunIR sweeps and the IR read
// mediation resolves the owning run from — and under no other run's. Its
// spelling is fixed: ir/<run>-bundle-ir-<sha256>-<32 lowercase hex>.json.
func TestContentAddressedIRKeyStaysInTheRunsBundleFamily(t *testing.T) {
	body := []byte(`{"program":"a"}`)
	id := irContentID(t, "run-a", body)
	key, err := IRBlobKey(id)
	if err != nil {
		t.Fatalf("IRBlobKey(%q): %v", id, err)
	}
	sum := sha256.Sum256(body)
	grammar := regexp.MustCompile(`^ir/run-a-bundle-ir-` + hex.EncodeToString(sum[:]) + `-[0-9a-f]{32}\.json$`)
	if !grammar.MatchString(key) {
		t.Fatalf("content key %q is not ir/run-a-bundle-ir-<sha256>-<32 lowercase hex>.json", key)
	}
	if got, err := validateIRBlobKey(key); err != nil || got != key {
		t.Fatalf("validateIRBlobKey(%q) = %q, %v: a runner refuses the publisher's key", key, got, err)
	}
	if got, err := IRBlobKeyRunID(key); err != nil || got != id {
		t.Fatalf("IRBlobKeyRunID(%q) = %q, %v, want %q", key, got, err, id)
	}
	own, err := IRBundleSnapshotPrefix("run-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, own) {
		t.Fatalf("content key %q is outside run-a's %q family: DeleteRunIR leaves it behind and the IR read mediation cannot attribute it", key, own)
	}
	if owner := id[:strings.LastIndex(id, "-bundle-")]; owner != "run-a" {
		t.Fatalf("the owner before the last -bundle- of %q is %q, want run-a", id, owner)
	}
	foreign, err := IRBundleSnapshotPrefix("run-a-b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(key, foreign) {
		t.Fatalf("content key %q falls under run-a-b's family %q", key, foreign)
	}
}

// Through a gateway that pages for real: the S3 client PUTs a compiled IR
// under its content key, three publications of one run — two of them of
// identical bytes — keep three objects, a GET by each wire key returns that
// publication's bytes, and DeleteRunIR takes them all.
func TestS3ContentAddressedIRRoundTripAndSweep(t *testing.T) {
	f, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	ctx := context.Background()
	bodies := [][]byte{[]byte(`{"program":"first"}`), []byte(`{"program":"later"}`), []byte(`{"program":"later"}`)}
	var keys []string
	seen := map[string]bool{}
	for _, body := range bodies {
		id := irContentID(t, "run-a", body)
		if err := c.PutIRBlob(ctx, id, body); err != nil {
			t.Fatalf("PutIRBlob(%q): %v", id, err)
		}
		key, err := IRBlobKey(id)
		if err != nil {
			t.Fatal(err)
		}
		if seen[key] {
			t.Fatalf("two publications share the key %q", key)
		}
		seen[key] = true
		keys = append(keys, key)
	}
	for i, key := range keys {
		got, err := c.GetIRBlob(ctx, key)
		if err != nil || !bytes.Equal(got, bodies[i]) {
			t.Fatalf("GetIRBlob(%q) = %q, %v, want %q", key, got, err, bodies[i])
		}
	}
	seed(f, foreignKeys...)
	if err := c.DeleteRunIR(ctx, "run-a"); err != nil {
		t.Fatalf("DeleteRunIR: %v", err)
	}
	for _, key := range keys {
		if f.has(key) {
			t.Fatalf("DeleteRunIR left %s in the bucket", key)
		}
		if _, err := c.GetIRBlob(ctx, key); !errors.Is(err, ErrArtifactNotFound) {
			t.Fatalf("GetIRBlob(%q) after DeleteRunIR: %v, want ErrArtifactNotFound", key, err)
		}
	}
	for _, k := range foreignKeys {
		if !f.has(k) {
			t.Fatalf("DeleteRunIR deleted %s, which is not run-a's", k)
		}
	}
}

// A refused IR key is told the shape every IR key has, ir/<logical_id>.json:
// the publisher writes content ids, not the legacy per-run key.
func TestIRBlobKeyRefusalNamesTheLogicalIDShape(t *testing.T) {
	key, err := IRBlobKey(irContentID(t, "run-a", []byte(`{"program":"a"}`)))
	if err != nil {
		t.Fatalf("IRBlobKey: %v", err)
	}
	_, err = IRBlobKeyRunID(strings.TrimSuffix(key, ".json"))
	if err == nil || !strings.Contains(err.Error(), "(want ir/<logical_id>.json)") {
		t.Fatalf("IRBlobKeyRunID of a key without its .json = %v, want a refusal naming ir/<logical_id>.json", err)
	}
}

// No two run ids the reservation accepts (store.RunIDCarriesIRBlobFamilyInfix)
// reach each other's IR objects: what DeleteRunIR deletes for one — its
// legacy key and everything under its IRBundleSnapshotPrefix — holds no
// object of the other, neither its legacy key, nor a bundle snapshot, nor a
// compiled IR's content key. Checked over every id of up to five tokens
// that spell and border the infix, so "x" beside "x-bundle" is among them.
func TestNoAcceptedRunIDReachesAnotherRunsIRObjects(t *testing.T) {
	tokens := []string{"x", "-", "bundle", "ir"}
	var ids []string
	var build func(prefix string, depth int)
	build = func(prefix string, depth int) {
		if prefix != "" && !store.RunIDCarriesIRBlobFamilyInfix(prefix) {
			ids = append(ids, prefix)
		}
		if depth == 0 {
			return
		}
		for _, tok := range tokens {
			build(prefix+tok, depth-1)
		}
	}
	build("", 5)
	digest := strings.Repeat("ab", 32)
	type reach struct{ legacy, sweep string }
	reaches := make([]reach, len(ids))
	objects := make([][]string, len(ids))
	for i, id := range ids {
		legacy, err := IRBlobKey(id)
		if err != nil {
			t.Fatalf("IRBlobKey(%q): %v", id, err)
		}
		sweep, err := IRBundleSnapshotPrefix(id)
		if err != nil {
			t.Fatalf("IRBundleSnapshotPrefix(%q): %v", id, err)
		}
		reaches[i] = reach{legacy, sweep}
		content, err := store.IRContentLogicalID(id, digest, bytes.NewReader(make([]byte, 16)))
		if err != nil {
			t.Fatalf("IRContentLogicalID(%q): %v", id, err)
		}
		for _, logical := range []string{id, id + store.IRBlobFamilyInfix + digest, content} {
			key, err := IRBlobKey(logical)
			if err != nil {
				t.Fatalf("IRBlobKey(%q): %v", logical, err)
			}
			objects[i] = append(objects[i], key)
		}
	}
	for i, deleted := range ids {
		for j, owner := range ids {
			if i == j {
				continue
			}
			for _, key := range objects[j] {
				if key == reaches[i].legacy || strings.HasPrefix(key, reaches[i].sweep) {
					t.Fatalf("DeleteRunIR(%q) deletes %s, an IR object of run %q", deleted, key, owner)
				}
			}
		}
	}
}
