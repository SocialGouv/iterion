package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// OrphanStaleAfter is how long a status=running run's events.jsonl must
// be untouched before PromoteStaleOrphans considers it abandoned.
//
// Live engines flush events continuously (tool_started / tool_called /
// llm_request / …); a freeze means the engine subprocess has died.
// 60s is conservative enough to avoid false-positives on healthy runs
// (even a long Anthropic reasoning turn streams progress events) and
// short enough that an operator who just suffered a watchexec restart
// can call `iterion resume` immediately after the studio comes back,
// rather than waiting out the 15-min staleRunCutoff used by the runs
// LIST filter (`pkg/server/runs_global.go`).
const OrphanStaleAfter = 60 * time.Second

// OrphanPromotion captures the outcome of a single auto-promotion so
// callers can log / surface it. RunID identifies the affected run,
// FromStatus is what it was on disk (always "running" today), and
// Reason describes the heuristic that classified it as orphaned.
type OrphanPromotion struct {
	RunID      string
	FromStatus RunStatus
	Reason     string
}

// PromoteStaleOrphans scans every runs/<id>/run.json under the store
// root and promotes any whose status=running AND whose events.jsonl
// mtime is older than OrphanStaleAfter to status=failed_resumable.
// Returns the list of promoted runs (empty when nothing changed).
//
// This closes the gap left by abnormal engine exits — watchexec
// rebuilds during dev, OS kills, OOMs, studio crashes — which leave
// run.json frozen at status=running. The CLI resume gate
// (`pkg/cli/resume.go`) rejects status=running, so without this sweep
// operators must hand-edit run.json to recover. By promoting at server
// boot we make `iterion resume` Just Work for the common post-crash
// case.
//
// Idempotent: a run already at any non-running status is skipped.
// Best-effort: per-run errors (unreadable file, bad JSON, CAS conflict)
// are logged at warn and don't abort the sweep.
//
// Safe to call concurrently with engine writes: UpdateRunStatusIf is
// CAS-guarded on status=running so a live engine that flips to
// finished/failed concurrently wins. The mtime safety check means we
// only target runs that have been silent long enough that the engine
// process is almost certainly dead — but even a false positive resolves
// itself when the engine's next UpdateRunStatus overwrites our value.
func (s *FilesystemRunStore) PromoteStaleOrphans(ctx context.Context, logger *iterlog.Logger) ([]OrphanPromotion, error) {
	runsDir := filepath.Join(s.root, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no runs/ yet — nothing to promote
		}
		return nil, fmt.Errorf("store: read runs dir: %w", err)
	}

	var promoted []OrphanPromotion
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runID := e.Name()

		r, loadErr := s.loadRunRaw(runID)
		if loadErr != nil {
			if logger != nil {
				logger.Warn("orphan sweep: load %s: %v", runID, loadErr)
			}
			continue
		}
		if r.Status != RunStatusRunning {
			continue
		}

		// Liveness check via events.jsonl mtime. The boot-time
		// invariant ("studio just started, no engine can be alive
		// yet") would technically let us promote without the mtime
		// check, but a parallel `iterion run` from CLI is independent
		// of the studio process — a 60s freshness window keeps that
		// case safe while still triggering on dead engines.
		evPath := filepath.Join(runsDir, runID, "events.jsonl")
		st, statErr := os.Stat(evPath)
		if statErr == nil && now.Sub(st.ModTime()) < OrphanStaleAfter {
			continue
		}
		reason := fmt.Sprintf("engine subprocess died abnormally; events.jsonl untouched ≥ %s (promoted at server boot — see docs/resume.md)", OrphanStaleAfter)
		if statErr != nil {
			reason = "engine subprocess died abnormally; events.jsonl missing (promoted at server boot)"
		}

		changed, casErr := s.UpdateRunStatusIf(ctx, runID, RunStatusFailedResumable, reason, []RunStatus{RunStatusRunning})
		if casErr != nil {
			if logger != nil {
				logger.Warn("orphan sweep: promote %s: %v", runID, casErr)
			}
			continue
		}
		if !changed {
			// Engine raced us — flipped to something else after our
			// loadRunRaw. That's the happy path: don't log noise.
			continue
		}
		promoted = append(promoted, OrphanPromotion{
			RunID:      runID,
			FromStatus: RunStatusRunning,
			Reason:     reason,
		})
		if logger != nil {
			logger.Info("orphan sweep: %s running → failed_resumable (%s)", runID, reason)
		}
	}
	return promoted, nil
}
