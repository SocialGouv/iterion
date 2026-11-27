package blob

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// artifactKey builds the canonical S3 key for an artifact body.
// Format: artifacts/<run_id>/<node_id>/<version>.json.
//
// Returns a descriptive error when any component fails sanitisation
// (path separator, traversal, control chars) or when version is
// negative. Callers in the blob package are all already in a fallible
// context (PutArtifact / GetArtifact / migration tooling), so
// surfacing the error is cheaper than the previous panic + recover
// dance and avoids killing the whole request-path goroutine when a
// malformed runID slips through the upstream sanitiser.
func artifactKey(runID, nodeID string, version int) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	if err := store.SanitizePathComponent("node_id", nodeID); err != nil {
		return "", fmt.Errorf("blob: invalid node_id: %w", err)
	}
	if version < 0 {
		return "", fmt.Errorf("blob: negative artifact version %d", version)
	}
	return fmt.Sprintf("artifacts/%s/%s/%d.json", runID, nodeID, version), nil
}

// attachmentKey builds the canonical S3 key for an attachment body.
// Format: attachments/<run_id>/<name>/<filename>.
//
// All three components are sanitised against the same "no separators,
// no control chars, no traversal" invariant as artifactKey — the
// FS-mirror path uses filepath.Base for the filename so keeping the
// S3 key shape consistent prevents `migrate to-cloud` from producing
// keys the FS layer would have flattened (F-ST-10).
func attachmentKey(runID, name, filename string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	if err := store.SanitizePathComponent("attachment_name", name); err != nil {
		return "", fmt.Errorf("blob: invalid attachment_name: %w", err)
	}
	if err := store.SanitizePathComponent("attachment_filename", filename); err != nil {
		return "", fmt.Errorf("blob: invalid attachment_filename: %w", err)
	}
	return fmt.Sprintf("attachments/%s/%s/%s", runID, name, filename), nil
}

// artifactRunPrefix is the S3 key prefix containing every artifact of a
// run. Trailing slash is included so a delete-by-prefix doesn't
// accidentally match `artifacts/<runID>-other/`.
func artifactRunPrefix(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("artifacts/%s/", runID), nil
}

// attachmentRunPrefix is the S3 key prefix containing every
// attachment for a run. Trailing slash is included so a delete-by-
// prefix doesn't accidentally match `attachments/<runID>-other/`.
func attachmentRunPrefix(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("attachments/%s/", runID), nil
}

// uploadKey is the blob key of a staged upload's bytes:
// uploads/<upload_id>/<filename>. A staged upload belongs to no run yet, so
// its family is keyed by the upload id the server minted; the record that
// scopes it to a tenant lives in the run store, and nothing presigns it.
func uploadKey(uploadID, filename string) (string, error) {
	if err := store.SanitizePathComponent("upload_id", uploadID); err != nil {
		return "", fmt.Errorf("blob: invalid upload_id: %w", err)
	}
	if err := store.SanitizePathComponent("upload_filename", filename); err != nil {
		return "", fmt.Errorf("blob: invalid upload_filename: %w", err)
	}
	return fmt.Sprintf("uploads/%s/%s", uploadID, filename), nil
}

// uploadPrefix is the blob key prefix holding one staged upload. The
// trailing slash keeps `uploads/<id>-other/` out of a sweep.
func uploadPrefix(uploadID string) (string, error) {
	if err := store.SanitizePathComponent("upload_id", uploadID); err != nil {
		return "", fmt.Errorf("blob: invalid upload_id: %w", err)
	}
	return fmt.Sprintf("uploads/%s/", uploadID), nil
}

// toolBlobKey builds the canonical S3 key for a per-tool-call I/O body.
// Format: tools/<run_id>/<tool_use_id>/<kind>, where kind ∈
// {input,output}. Mirrors the filesystem store's
// runs/<id>/tools/<toolUseID>/<kind> layout so `migrate to-cloud` can
// copy bytes across without rewriting paths. kind is validated by the
// caller (store layer) before this point; it is still sanitised here as
// a path component so a malformed value can never escape the prefix.
func toolBlobKey(runID, toolUseID, kind string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	if err := store.SanitizePathComponent("tool_use_id", toolUseID); err != nil {
		return "", fmt.Errorf("blob: invalid tool_use_id: %w", err)
	}
	if kind != "input" && kind != "output" {
		return "", fmt.Errorf("blob: tool blob kind must be input|output, got %q", kind)
	}
	return fmt.Sprintf("tools/%s/%s/%s", runID, toolUseID, kind), nil
}

// irBlobKey builds the canonical S3 key for an out-of-band IR blob:
// ir/<logical_id>.json. The logical id is the run id — a legacy key that
// runners read and publishers do not write — or one of the run's "-bundle-"
// ids (IRBundleSnapshotPrefix): a compiled IR's content id
// (store.IRContentLogicalID) or a bundle snapshot. It is one path
// component, so there is no sub-path to sanitise beyond it.
func irBlobKey(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("ir/%s.json", runID), nil
}

// IRBundleSnapshotPrefix is the S3 key prefix of a run's "-bundle-" family
// of IR objects: the immutable bundle snapshots, parked under the IR key of
// "<run_id>-bundle-<digest>" (ir/<run_id>-bundle-<digest>.json), and the
// compiled IRs, under "<run_id>-bundle-ir-<digest>-<nonce>"
// (store.IRContentLogicalID), each when it exceeds the queue payload. Keyed
// by the run id like every run key space, so DeleteRunIR sweeps them with
// the run's legacy ir/<run_id>.json. The run id is sanitised, and the
// "-bundle-" suffix (store.IRBlobFamilyInfix) keeps the prefix off another
// run whose id merely starts with this one — the same infix the IR read
// mediation splits on (store/mongo GetIRBlob). That holds while no run id
// carries the infix or ends with "-bundle" — the prefix of run "x" covers
// the whole family of run "x-bundle" — and the doors that take a run id
// from a caller into the cloud store refuse both shapes
// (store.RunIDCarriesIRBlobFamilyInfix).
func IRBundleSnapshotPrefix(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return "ir/" + runID + store.IRBlobFamilyInfix, nil
}

// validateIRBlobKey re-derives the canonical IR key from a storage key
// carried on the wire (queue.IRRef.StorageKey) and confirms they match,
// so a tampered or malformed reference can never escape the ir/ prefix.
// Returns the (identical) canonical key on success.
func validateIRBlobKey(storageKey string) (string, error) {
	runID := strings.TrimSuffix(strings.TrimPrefix(storageKey, "ir/"), ".json")
	canonical, err := irBlobKey(runID)
	if err != nil {
		return "", err
	}
	if canonical != storageKey {
		return "", fmt.Errorf("blob: invalid IR storage key %q (want ir/<logical_id>.json)", storageKey)
	}
	return canonical, nil
}

// IRBlobKeyRunID returns the logical id embedded in a canonical IR storage
// key: the run id of "ir/<run_id>.json", or the "<run_id>-bundle-…" id of a
// bundle snapshot or of a compiled IR (store.IRContentLogicalID). The mongo
// IR blob read uses it to bind the fetch to the owning run's tenant (D14 F7).
func IRBlobKeyRunID(storageKey string) (string, error) {
	if _, err := validateIRBlobKey(storageKey); err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimPrefix(storageKey, "ir/"), ".json"), nil
}

// toolBlobRunPrefix is the S3 key prefix containing every tool blob for
// a run. Trailing slash guards against matching `tools/<runID>-other/`.
func toolBlobRunPrefix(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("tools/%s/", runID), nil
}

// backendSessionKey is sessions/<run_id>/<ref>.
func backendSessionKey(runID, ref string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	if err := store.SanitizePathComponent("session_ref", ref); err != nil {
		return "", fmt.Errorf("blob: invalid session_ref: %w", err)
	}
	return fmt.Sprintf("sessions/%s/%s", runID, ref), nil
}

func backendSessionRunPrefix(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("sessions/%s/", runID), nil
}

// runFileKey builds the canonical S3 key for a tool-produced artifact
// file: runfiles/<run_id>/<rel_path>. Unlike the other blobs, rel_path
// is a MULTI-segment path (tools may drop nested dirs, e.g.
// "renovacy/dbug-1.0-to-1.7.md"), so each segment is sanitised
// independently and traversal ("..", absolute, empty) is rejected — the
// same containment invariant the filesystem store's OpenRunFile enforces
// via its openat walk, applied here to the flat S3 key space.
func runFileKey(runID, relPath string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	clean, err := sanitizeRunFileRelPath(relPath)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("runfiles/%s/%s", runID, clean), nil
}

// runFileRunPrefix is the S3 key prefix containing every artifact file
// for a run. Trailing slash guards against matching `runfiles/<runID>-x/`.
func runFileRunPrefix(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("runfiles/%s/", runID), nil
}

// sanitizeRunFileRelPath validates a slash-separated relative path and
// returns its cleaned form. Rejects absolute paths, empty input, and any
// "." / ".." / empty / backslash-bearing segment, then sanitises each
// segment against the standard path-component invariant.
func sanitizeRunFileRelPath(relPath string) (string, error) {
	slashPath := strings.ReplaceAll(relPath, "\\", "/")
	if slashPath == "" || strings.HasPrefix(slashPath, "/") {
		return "", fmt.Errorf("blob: invalid run file path %q", relPath)
	}
	segments := strings.Split(slashPath, "/")
	clean := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("blob: invalid run file path %q", relPath)
		}
		if err := store.SanitizePathComponent("run_file_segment", seg); err != nil {
			return "", fmt.Errorf("blob: invalid run file path %q: %w", relPath, err)
		}
		clean = append(clean, seg)
	}
	return strings.Join(clean, "/"), nil
}
