package runtime

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// reviewMediaRefsKey is the structured field a review companion may return,
// or an upstream node may pass into an ordinary human node's input. The
// historical wire name covers both media and passive document/data
// attachments. Values are untrusted until normalizeReviewMediaRefs matches
// them against the run's actual artifact_files manifest.
const reviewMediaRefsKey = "media_refs"

const (
	maxReviewMediaInventory = 1000
	maxReviewMediaPerTurn   = 12
	maxReviewMediaCaption   = 500
)

// availableReviewMedia returns the passive review attachments currently
// produced by this run. Besides browser media, this includes a small allowlist
// of documents and structured-text formats that the Studio can render as text
// or an inert document preview. It merges the durable read side with the local
// scratch directory: for the filesystem store they are the same files, while a
// cloud runner's S3 read side is only flushed after the engine returns at a
// pause.
func (e *Engine) availableReviewMedia(rs *runState) []store.ReviewMediaRef {
	if e == nil || rs == nil {
		return nil
	}
	rfs := store.AsRunFilesStore(e.store)
	if rfs == nil {
		return nil
	}

	byPath := make(map[string]store.ReviewMediaRef)
	if files, err := rfs.ListRunFiles(rs.ctx, rs.runID); err == nil {
		for _, f := range files {
			addReviewMediaFile(byPath, rs.runID, f.Path, f.Size)
		}
	} else if e.logger != nil {
		e.logger.Warn("runtime: list review media for run %s: %v", rs.runID, err)
	}

	// EnsureRunFilesDir is idempotent and, by contract, returns the local
	// scratch path mounted into the sandbox. Walking it lets a cloud review
	// companion see files written earlier in the same engine pass, before the
	// runner uploads them to object storage.
	if dir, err := rfs.EnsureRunFilesDir(rs.ctx, rs.runID); err == nil && dir != "" {
		_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil // one unreadable entry must not block the human gate
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return nil
			}
			addReviewMediaFile(byPath, rs.runID, filepath.ToSlash(rel), info.Size())
			return nil
		})
	} else if err != nil && e.logger != nil {
		e.logger.Warn("runtime: locate review media scratch dir for run %s: %v", rs.runID, err)
	}

	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > maxReviewMediaInventory {
		paths = paths[:maxReviewMediaInventory]
	}
	out := make([]store.ReviewMediaRef, 0, len(paths))
	for _, path := range paths {
		out = append(out, byPath[path])
	}
	return out
}

func addReviewMediaFile(dst map[string]store.ReviewMediaRef, runID, path string, size int64) {
	kind, mime, ok := reviewMediaType(path)
	if !ok || path == "" {
		return
	}
	dst[path] = store.ReviewMediaRef{
		RunID: runID,
		Path:  path,
		Kind:  kind,
		MIME:  mime,
		Size:  size,
	}
}

// reviewMediaType deliberately allows only passive/browser-preview formats.
// SVG and HTML are excluded from review attachments even though the generic
// artifact browser can download them: they are active document formats and
// must not be embedded next to an approval control. Unknown binaries remain
// available from the run's artifact browser but cannot be attached to a review.
func reviewMediaType(path string) (kind, mime string, ok bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image", "image/png", true
	case ".jpg", ".jpeg":
		return "image", "image/jpeg", true
	case ".gif":
		return "image", "image/gif", true
	case ".webp":
		return "image", "image/webp", true
	case ".wav":
		return "audio", "audio/wav", true
	case ".mp3":
		return "audio", "audio/mpeg", true
	case ".ogg", ".oga":
		return "audio", "audio/ogg", true
	case ".flac":
		return "audio", "audio/flac", true
	case ".m4a":
		return "audio", "audio/mp4", true
	case ".opus":
		return "audio", "audio/opus", true
	case ".mp4", ".m4v":
		return "video", "video/mp4", true
	case ".webm":
		return "video", "video/webm", true
	case ".mov":
		return "video", "video/quicktime", true
	case ".mkv":
		return "video", "video/x-matroska", true
	case ".json":
		return "data", "application/json", true
	case ".csv":
		return "data", "text/csv", true
	case ".yaml", ".yml":
		return "data", "application/yaml", true
	case ".md", ".markdown":
		return "doc", "text/markdown", true
	case ".txt":
		return "doc", "text/plain", true
	case ".pdf":
		return "doc", "application/pdf", true
	default:
		return "", "", false
	}
}

// normalizeReviewMediaRefs treats model/upstream output as a list of path +
// optional caption selections, then replaces every other field with metadata
// from the actual manifest. Unknown paths, foreign run ids, duplicates, URLs,
// data URIs, and non-media files therefore disappear rather than reaching the
// board as attacker-controlled embeds.
func normalizeReviewMediaRefs(raw any, available []store.ReviewMediaRef) []store.ReviewMediaRef {
	if raw == nil || len(available) == 0 {
		return nil
	}
	byPath := make(map[string]store.ReviewMediaRef, len(available))
	for _, ref := range available {
		byPath[ref.Path] = ref
	}

	type selection struct {
		path    string
		runID   string
		caption string
	}
	var selections []selection
	switch values := raw.(type) {
	case []store.ReviewMediaRef:
		selections = make([]selection, 0, len(values))
		for _, value := range values {
			selections = append(selections, selection{path: value.Path, runID: value.RunID, caption: value.Caption})
		}
	case []map[string]any:
		selections = make([]selection, 0, len(values))
		for _, value := range values {
			selections = append(selections, mediaSelectionFromMap(value))
		}
	case []any:
		selections = make([]selection, 0, len(values))
		for _, value := range values {
			if item, ok := value.(map[string]any); ok {
				selections = append(selections, mediaSelectionFromMap(item))
			}
		}
	default:
		return nil
	}

	seen := make(map[string]struct{}, len(selections))
	out := make([]store.ReviewMediaRef, 0, min(len(selections), maxReviewMediaPerTurn))
	for _, selected := range selections {
		path := strings.TrimSpace(selected.path)
		manifest, exists := byPath[path]
		if !exists || (selected.runID != "" && selected.runID != manifest.RunID) {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		manifest.Caption = truncateRunes(strings.TrimSpace(selected.caption), maxReviewMediaCaption)
		out = append(out, manifest)
		if len(out) == maxReviewMediaPerTurn {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mediaSelectionFromMap(value map[string]any) (out struct {
	path    string
	runID   string
	caption string
}) {
	out.path, _ = value["path"].(string)
	out.runID, _ = value["run_id"].(string)
	out.caption, _ = value["caption"].(string)
	return out
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// reviewMediaForPause resolves either the companion-provided `media` event
// field or an ordinary human node's input.media_refs against the current run.
// The historical media_refs name is retained on the wire for compatibility,
// but refs may now point to passive documents/data as well as browser media.
func (e *Engine) reviewMediaForPause(rs *runState, questions, eventExtra map[string]any) []store.ReviewMediaRef {
	if eventExtra != nil {
		// Presence is authoritative, including an explicit empty selection from
		// a guided companion. Falling back to input.media_refs in that case
		// would re-attach files the companion deliberately omitted this turn.
		if raw, present := eventExtra["media"]; present {
			return normalizeReviewMediaRefs(raw, e.availableReviewMedia(rs))
		}
	}
	if questions == nil {
		return nil
	}
	raw := questions[reviewMediaRefsKey]
	return normalizeReviewMediaRefs(raw, e.availableReviewMedia(rs))
}

// flushReviewMediaForPause closes the cloud scratch→durable visibility gap.
// The board can observe PauseRun immediately, while the runner's normal upload
// happens only after Engine.Run/Resume returns ErrRunPaused. Uploading first
// guarantees an attachment URL rendered from the checkpoint is already
// readable. Filesystem stores have no uploader and no gap.
func (e *Engine) flushReviewMediaForPause(rs *runState, media []store.ReviewMediaRef) {
	if e == nil || rs == nil || len(media) == 0 {
		return
	}
	uploader := store.AsRunFilesUploader(e.store)
	if uploader == nil {
		return
	}
	if _, err := uploader.UploadRunFiles(rs.ctx, rs.runID); err != nil && e.logger != nil {
		// Review media is an enrichment. Preserve the human pause even if the
		// object-store bridge is temporarily unavailable; the runner's normal
		// post-return upload retries the same idempotent copy.
		e.logger.Warn("runtime: flush review media for run %s: %v", rs.runID, err)
	}
}
