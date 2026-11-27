package queue

import (
	"strings"
	"testing"
)

// IRDigest is the lowercase hex SHA-256 of the exact bytes: the spelling the
// publisher stamps on IRRef.Digest and the runner recomputes.
func TestIRDigestIsTheLowercaseHexSHA256OfTheBytes(t *testing.T) {
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := IRDigest(nil); got != emptySHA256 {
		t.Fatalf("IRDigest(nil) = %s, want %s", got, emptySHA256)
	}
	if IRDigest([]byte(`{"a":1}`)) == IRDigest([]byte(`{"a":2}`)) {
		t.Fatal("two programs share a digest")
	}
}

func irRefMessage(ref *IRRef) *RunMessage {
	return &RunMessage{V: SchemaVersion, RunID: "run_1", WorkflowName: "demo", IRRef: ref}
}

// A digest is optional on the wire — a legacy reference carries none — but
// one that is present must be the spelling the runner can compare: the
// queue refuses anything else at decode instead of letting the runner refuse
// every fetched byte as a mismatch.
func TestRunMessage_ValidateIRRefDigest(t *testing.T) {
	digest := IRDigest([]byte("program"))
	for _, tc := range []struct {
		name   string
		digest string
		ok     bool
	}{
		{"legacy reference without a digest", "", true},
		{"lowercase hex SHA-256", digest, true},
		{"uppercase hex", strings.ToUpper(digest), false},
		{"truncated", digest[:63], false},
		{"algorithm prefix", "sha256:" + digest[:57], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := irRefMessage(&IRRef{StorageKey: "ir/run_1-bundle-ir-" + digest + "-" + strings.Repeat("0f", 16) + ".json", Backend: IRBackendS3, Digest: tc.digest}).Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "IRRef.Digest")) {
				t.Fatalf("Validate accepted digest %q: %v", tc.digest, err)
			}
		})
	}
}

// A bundle snapshot's digest lives in snapshot_digest, which
// bundle.DecodeSnapshot checks; a snapshot reference that names ANOTHER
// digest would be a second, unread claim about the same bytes, so the queue
// refuses it. The same digest, or none, is accepted.
func TestRunMessage_ValidateSnapshotRefDigestMatchesSnapshotDigest(t *testing.T) {
	snapshot := IRDigest([]byte("snapshot"))
	for _, tc := range []struct {
		name   string
		digest string
		ok     bool
	}{
		{"no digest on the reference", "", true},
		{"the snapshot digest", snapshot, true},
		{"another digest", IRDigest([]byte("other")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &RunMessage{V: SchemaVersion, RunID: "run_1", WorkflowName: "demo", IRCompiled: []byte(`{}`), BotBundle: &BotBundleRef{
				SnapshotDigest: snapshot,
				SnapshotRef:    &IRRef{StorageKey: "ir/run_1-bundle-" + snapshot + ".json", Backend: IRBackendS3, Digest: tc.digest},
			}}
			err := m.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "differs from snapshot_digest")) {
				t.Fatalf("Validate accepted snapshot reference digest %q: %v", tc.digest, err)
			}
		})
	}
}
