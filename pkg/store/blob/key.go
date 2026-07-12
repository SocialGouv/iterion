package blob

import (
	"fmt"

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

// toolBlobRunPrefix is the S3 key prefix containing every tool blob for
// a run. Trailing slash guards against matching `tools/<runID>-other/`.
func toolBlobRunPrefix(runID string) (string, error) {
	if err := store.SanitizePathComponent("run_id", runID); err != nil {
		return "", fmt.Errorf("blob: invalid run_id: %w", err)
	}
	return fmt.Sprintf("tools/%s/", runID), nil
}
