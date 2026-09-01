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
// in the packages it walks — the three the contract card names plus the
// two where the motivating drift bug actually lived (pkg/runtime's
// cancel CAS, pkg/server/cloudpublisher's cancellable set) — every line
// combining two or more distinct RunStatus identifiers is a policy set
// and must be either the contract itself or an ALLOWLISTED,
// individually-justified divergence. The allowlist is keyed by
// file + status-combo, so a NEW set slipping into an already-known file
// still fails; an allowlist entry that stops matching fails too (stale
// justifications are how drift hides).
func TestNoHandRolledTerminalSets(t *testing.T) {
	root := "../.." // pkg/store → repo root
	pkgs := []string{"pkg/store", "pkg/supervise", "pkg/runview", "pkg/runtime", "pkg/server/cloudpublisher"}

	statusIdent := regexp.MustCompile(`RunStatus(Finished|Failed|FailedResumable|Cancelled|PausedWaitingHuman|PausedOperator|Running|Queued)\b`)
	// Slice literals span lines (the motivating publisher set did), so
	// they are matched as whole `[]RunStatus{...}` blocks; everything
	// else (case clauses, boolean chains) is effectively single-line.
	sliceLit := regexp.MustCompile(`(?s)\[\](?:store\.)?RunStatus\{[^}]*\}`)

	// One key = "<rel-path> :: <sorted status combo>", one value = the
	// reason this exact set is allowed to exist outside lifecycle.go.
	allow := map[string]string{}
	for k, v := range negativeSpaceAllowlist {
		allow[k] = v
	}

	found := map[string]bool{}
	for _, p := range pkgs {
		dir := filepath.Join(root, p)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			record := func(chunk string) {
				ms := statusIdent.FindAllString(chunk, -1)
				if len(ms) < 2 {
					return
				}
				set := map[string]bool{}
				for _, m := range ms {
					set[strings.TrimPrefix(m, "RunStatus")] = true
				}
				if len(set) < 2 {
					return
				}
				combo := make([]string, 0, len(set))
				for k := range set {
					combo = append(combo, k)
				}
				sort.Strings(combo)
				found[rel+" :: "+strings.Join(combo, "+")] = true
			}
			src := string(data)
			for _, lit := range sliceLit.FindAllString(src, -1) {
				record(lit)
			}
			for _, line := range strings.Split(sliceLit.ReplaceAllString(src, ""), "\n") {
				record(line)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", p, err)
		}
	}

	for key := range found {
		if strings.HasPrefix(key, "pkg/store/lifecycle.go ::") || strings.HasPrefix(key, "pkg/store/run.go ::") {
			continue // the contract and the predicates beside the enum
		}
		if _, ok := allow[key]; !ok {
			t.Errorf("hand-rolled RunStatus set: %q — replace it with a lifecycle.go predicate or allowlist it with its reason", key)
		}
	}
	for key, reason := range allow {
		if !found[key] {
			t.Errorf("stale allowlist entry %q (%s) — the set is gone; prune the entry", key, reason)
		}
	}
}

// negativeSpaceAllowlist: every surviving multi-status set outside the
// contract, each with the reason it is NOT a predicate call. Adding a
// set means arguing its reason here.
var negativeSpaceAllowlist = map[string]string{
	// -- pkg/store: transition machinery, not policy sets.
	"pkg/store/store_run.go :: Cancelled+Failed+FailedResumable+Finished":  "applyStatusTransition side-effect switch (FinishedAt stamping)",
	"pkg/store/store_run.go :: Finished+Running":                           "applyStatusTransition checkpoint-clear pair (running/finished drop the recovery point)",
	"pkg/store/store_run.go :: PausedWaitingHuman+Running":                 "applyStatusTransition FinishedAt-clear pair (resume paths un-freeze the duration ticker)",
	"pkg/store/mongo/runs.go :: Cancelled+Failed+FailedResumable+Finished": "runStatusUpdate: mongo twin of the transition side-effect switch",
	"pkg/store/mongo/runs.go :: Queued+Running":                            "CountsAgainstLaunchLimit twin inside a Mongo $in filter (CountActiveRunsByTenant); a bson filter cannot call a predicate",

	// -- pkg/runview: individually documented divergences.
	"pkg/runview/rewind.go :: Cancelled+Failed+FailedResumable+PausedOperator+PausedWaitingHuman+Queued": "rewindableStatuses: deliberately wider than CanOperatorResume (failed stays rewindable, queued claimable)",
	"pkg/runview/service_control.go :: FailedResumable+PausedOperator+PausedWaitingHuman":                "CancelInactive flippable arm (queued has its own case; failed deliberately excluded; no running — a live run cancels through its process)",
	"pkg/runview/service_control.go :: Cancelled+Failed+Finished":                                        "inbox refuse set: failed_resumable deliberately ACCEPTED (drained on resume)",
	"pkg/runview/service_lifecycle.go :: PausedWaitingHuman+Running":                                     "sandboxContainerReapable keep-set: documented wider-than-IsTerminal reaping (paused_operator reapable)",
	"pkg/runview/subbot.go :: Cancelled+Failed+FailedResumable":                                          "subbot outcome routing: failure-trio → clear + rerun fresh, a three-way outcome switch",

	// -- pkg/supervise: steering admission.
	"pkg/supervise/inproc.go :: Cancelled+Failed+Finished": "steering inbox refuse set: failed_resumable deliberately accepted (drained on resume)",

	// -- pkg/runtime: claim-CAS / routing sets (they gate a TRANSITION,
	// not external eligibility — see CanOperatorResume's doc).
	"pkg/runtime/run_failure.go :: FailedResumable+PausedOperator+PausedWaitingHuman+Running": "engine ctx-cancel CAS: CanBeCancelled minus queued — a queued doc is a NEWER attempt this engine does not own",
	"pkg/runtime/resume.go :: Cancelled+FailedResumable+PausedOperator":                       "Resume dispatch: routes the failure-shaped statuses to resumeFromFailure (the answers path is a separate case)",
	"pkg/runtime/resume.go :: Cancelled+FailedResumable+PausedOperator+Queued":                "failure-resume claim: CanOperatorResume minus paused_waiting_human (answers path) plus queued (cloud pre-flip)",
	"pkg/runtime/resume.go :: PausedWaitingHuman+Queued":                                      "pause-resume claim: queued accepted for the cloud pre-flip",
	"pkg/runtime/worktree.go :: Cancelled+Failed+Finished":                                    "RecoverFinalize gate: only fully-stopped shapes finalize; failed_resumable deliberately waits for resume-or-cancel",
}
