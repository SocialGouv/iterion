package store

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestNoHandRolledTerminalSets is the negative-space guard of ADR-095:
// in the three packages the contract card names (pkg/store,
// pkg/supervise, pkg/runview), every multi-status RunStatus set —
// a `case` clause or a []RunStatus literal combining two or more
// terminal-ish statuses — must either live in lifecycle.go or be an
// ALLOWLISTED, individually-justified divergence. A new hand-rolled set
// fails here until it is either replaced by a predicate or argued onto
// the allowlist.
func TestNoHandRolledTerminalSets(t *testing.T) {
	root := "../.." // pkg/store → repo root
	pkgs := []string{"pkg/store", "pkg/supervise", "pkg/runview"}

	// Statuses whose combination signals a policy set. running/queued
	// pairs are launch-capacity sets and equally policy-bearing.
	statusIdent := regexp.MustCompile(`RunStatus(Finished|Failed|FailedResumable|Cancelled|PausedWaitingHuman|PausedOperator|Running|Queued)\b`)

	// One detection = "<rel-path>: <sorted status combo>".
	// The allowlist maps each accepted detection to its reason; the
	// reason is printed when the corresponding line disappears so a
	// stale entry is noticed too.
	allow := map[string]string{
		// -- pkg/store: the contract itself and its machinery.
		"pkg/store/lifecycle.go":             "the canonical contract",
		"pkg/store/lifecycle_test.go":        "the contract's truth table",
		"pkg/store/store_run.go":             "transition machinery (applyStatusTransition side-effect switch + cancelled-wins guards), not a policy set",
		"pkg/store/mongo/runs.go":            "mongo twin of the transition machinery ($set side-effect switches + notifiable filter, kept in lockstep by conformance)",
		"pkg/store/storetest/conformance.go": "conformance harness exercising transitions",
		"pkg/store/run.go":                   "IsTerminal/IsPaused: contract predicates declared beside the enum",
		// -- pkg/runview: individually documented divergences.
		"pkg/runview/rewind.go":               "rewindableStatuses: deliberately wider than CanOperatorResume (failed kept rewindable, queued claimable)",
		"pkg/runview/rewind_files.go":         "restore path branches on finished/failed/rewindable — mirrors rewindableStatuses",
		"pkg/runview/service_control.go":      "CancelInactive flippable set (failed deliberately excluded) + inbox refuse set (failed_resumable deliberately accepted)",
		"pkg/runview/service_launch.go":       "validateResumable: delegates to CanOperatorResume; retains the answers-path split",
		"pkg/runview/service_lifecycle.go":    "sandboxContainerReapable: documented wider-than-IsTerminal set (paused_operator reapable)",
		"pkg/runview/subbot.go":               "subbot outcome routing: finished vs failure-trio vs in-flight, a three-way outcome switch",
		"pkg/runview/pipeline_scheduler.go":   "queued-only launch guards",
		"pkg/runview/service_runs.go":         "list filters",
		"pkg/runview/snapshot.go":             "ExecStatus (node-row) vocabulary, not RunStatus policy",
		"pkg/runview/service_interactions.go": "paused_waiting_human auto-resume trigger, single-status",
		"pkg/runview/service_commands.go":     "AnswerHuman requires paused_waiting_human, single-status",
		"pkg/runview/detached.go":             "IsPaused stream-keepalive, single-status checks",
		"pkg/runview/fork.go":                 "fork shell parked as synthetic cancelled (empty reason and code)",
		// -- pkg/supervise: event-level vocabulary, not RunStatus.
		"pkg/supervise/inproc.go": "steering inbox refuse set: finished/failed/cancelled refuse, failed_resumable deliberately accepted (drained on resume)",
	}

	found := map[string][]string{}
	for _, p := range pkgs {
		dir := filepath.Join(root, p)
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if strings.HasSuffix(rel, "_test.go") && rel != "pkg/store/lifecycle_test.go" {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, line := range strings.Split(string(data), "\n") {
				ms := statusIdent.FindAllString(line, -1)
				if len(ms) < 2 {
					continue
				}
				set := map[string]bool{}
				for _, m := range ms {
					set[m] = true
				}
				if len(set) < 2 {
					continue
				}
				combo := make([]string, 0, len(set))
				for k := range set {
					combo = append(combo, k)
				}
				sort.Strings(combo)
				found[filepath.ToSlash(rel)] = append(found[filepath.ToSlash(rel)], strings.Join(combo, "+"))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", p, err)
		}
	}

	for file, combos := range found {
		if _, ok := allow[file]; !ok {
			t.Errorf("hand-rolled RunStatus set in %s (%v) — replace with a lifecycle.go predicate or allowlist it here with its reason", file, combos)
		}
	}
	for file, reason := range allow {
		if _, ok := found[file]; !ok && !strings.Contains(reason, "contract") {
			t.Logf("allowlist entry %s no longer matches any set (%s) — consider pruning", file, reason)
		}
	}
}
