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

// attachmentRunPrefix is the S3 key prefix containing every
// attachment for a run. Trailing slash is included so a delete-by-
// prefix doesn't accidentally match `attachments/<runID>-other/`.
func attachmentRunPrefix(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("attachments/%s/", runID), nil
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

// irBlobKey builds the canonical S3 key for an out-of-band compiled IR:
// ir/<run_id>.json. One object per run (the IRRef fallback only ever
// stashes a single IR per run), so there is no sub-path to sanitise
// beyond the run id.
func irBlobKey(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("ir/%s.json", runID), nil
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
		return "", fmt.Errorf("blob: invalid IR storage key %q (want ir/<run_id>.json)", storageKey)
	}
	return canonical, nil
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
