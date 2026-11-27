package store

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// IRBlobStore is an optional interface a store implements to stash an
// oversized compiled IR out-of-band, under a logical id the run owns. It is
// the storage half of the queue's IRRef fallback: when a workflow's
// marshaled IR exceeds the NATS max_payload (default 1 MiB), the cloud
// publisher parks the IR here under its content id (IRContentLogicalID) and
// sends a lightweight queue.IRRef on the RunMessage; the runner fetches it
// back and checks its digest before compiling.
//
// The cloud (Mongo) store satisfies it, backed by the same S3 blob client
// that holds artifacts + tool blobs (IR blobs can exceed the 16 MiB BSON
// ceiling, so they live in S3, not Mongo). Filesystem / local stores do
// NOT implement it — the local runtime never crosses the queue, so an
// oversized-IR offload cannot arise there. Callers MUST nil-check via
// AsIRBlobStore.
type IRBlobStore interface {
	// PutIRBlob stashes body under the deterministic key derived from
	// logicalID and returns that key (which travels on
	// queue.IRRef.StorageKey). logicalID is the run id or one of the run's
	// "-bundle-" ids: a compiled IR's content id (IRContentLogicalID) or a
	// bundle snapshot's "<run_id>-bundle-<digest>". Idempotent: re-writing
	// the same logical id replaces the prior bytes.
	PutIRBlob(ctx context.Context, logicalID string, body []byte) (storageKey string, err error)
	// GetIRBlob returns the bytes previously stashed under storageKey.
	// A missing blob returns an os.ErrNotExist-compatible error.
	GetIRBlob(ctx context.Context, storageKey string) ([]byte, error)
	// IRBlobBackend names the backend the blobs live in ("s3" | "mongo"),
	// matching the queue.IRBackend enum the publisher stamps on the IRRef.
	IRBlobBackend() string
}

// AsIRBlobStore returns s as IRBlobStore when the backend can host
// out-of-band IR blobs, or nil otherwise. Callers MUST nil-check
// (filesystem / local stores return nil).
func AsIRBlobStore(s RunStore) IRBlobStore {
	return capability[IRBlobStore](s)
}

// IRBlobFamilyInfix separates a run id from the suffix of each IR blob id
// the run owns beside its own: a bundle snapshot's "<run_id>-bundle-<digest>"
// and a compiled IR's content id (IRContentLogicalID). The IR key space reads
// the owning run before the last infix (the tenant mediation of an IR read)
// and sweeps the family under "ir/<run_id>-bundle-" (DeleteRunIR). Two run id
// shapes break that, and RunIDCarriesIRBlobFamilyInfix names both: an id
// carrying the infix names another run's IR object as its own legacy key
// ir/<run_id>.json — its tenant could read that object, and deleting the run
// would delete it — and an id ending with "-bundle" puts its whole family
// under the sweep prefix of the run named before that ending, so deleting
// run "x" would delete the IR objects of run "x-bundle".
const IRBlobFamilyInfix = "-bundle-"

// RunIDCarriesIRBlobFamilyInfix reports whether runID carries
// IRBlobFamilyInfix or ends with "-bundle", the infix without its closing
// "-": the two shapes no run id of a store with an IR blob key space may
// take (IRBlobFamilyInfix). "-" is the infix's only border, so no other id
// lets one run's sweep prefix or owner split reach another run's IR object.
// The ids the platform mints never take either shape (a GenerateRunID
// UUIDv7, a submission's "idem-" id, a delegation's "repair-" id). The doors
// that take a run id from a caller refuse one that does where an IR blob key
// space exists: runview.Launch on a store that has one (store.AsIRBlobStore),
// and the migration of a local run to the cloud store. A local run id may
// take either shape: a filesystem store has no IR blob key space.
func RunIDCarriesIRBlobFamilyInfix(runID string) bool {
	return strings.Contains(runID+"-", IRBlobFamilyInfix)
}

// irContentNonceBytes is the size of the nonce IRContentLogicalID draws:
// 128 bits.
const irContentNonceBytes = 16

// IRContentLogicalID is the logical id a publisher stashes one publication's
// compiled IR under: "<run_id>-bundle-ir-<digest>-<nonce>", digest being the
// lowercase hex SHA-256 of the exact bytes (queue.IRDigest) and nonce 128
// bits read from random, in 32 lowercase hex digits. The publisher calls it
// once per publication with crypto/rand.Reader, so every publication of a
// run — a resume, a re-publish after a reverted queued flip, even of
// identical bytes — writes its own object and never replaces the bytes an
// earlier message still points at.
//
// The nonce also makes the id unguessable, so no caller can have a server
// of an earlier release — mid-rollout, or after a rollback — overwrite it
// through a guessed run id: that server refuses no run id, and its
// publisher stashes a run's oversized IR under ir/<run_id>.json, so a run
// named after this id would have its program written there. No caller can
// predict the id: it exists from the publication on, carried by the run's
// own queue message, and named only in operator logs and in the run's own
// error when loading it fails. A failed or short read of random is an
// error: the id is never built from a predictable nonce.
//
// It lives in the run's "-bundle-" family on purpose, so the release before
// it serves it unchanged: the IR key validation accepts it
// (blob.IRBlobKeyRunID), the tenant mediation of an IR read resolves the
// owning run before the last "-bundle-" (store/mongo GetIRBlob), and
// DeleteRunIR sweeps it under blob.IRBundleSnapshotPrefix. The "ir-" infix
// keeps it apart from a bundle snapshot, whose suffix is a bare digest.
func IRContentLogicalID(runID, digest string, random io.Reader) (string, error) {
	if err := SanitizePathComponent("run_id", runID); err != nil {
		return "", err
	}
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", fmt.Errorf("store: IR content digest %q is not a lowercase hex SHA-256", digest)
	}
	var nonce [irContentNonceBytes]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return "", fmt.Errorf("store: draw the IR content id nonce of run %s: %w", runID, err)
	}
	return runID + IRBlobFamilyInfix + "ir-" + digest + "-" + hex.EncodeToString(nonce[:]), nil
}
