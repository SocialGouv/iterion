package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoHandRolledTerminalSets is the negative-space guard of ADR-095:
// in the packages it walks — the three the contract card names, the two
// where the motivating drift bug lived (pkg/runtime, cloudpublisher),
// and every package part 4 migrated — any SYNTACTIC construct grouping
// two or more distinct RunStatus identifiers (a case clause, any
// composite literal — []RunStatus, []string, bson.A, a map — or a
// maximal &&/|| chain) is a policy set. It must be either the contract
// itself or an ALLOWLISTED, individually-justified divergence. The
// allowlist is keyed by file + status-combo AND carries the expected
// occurrence count, so a SECOND set with an already-justified combo in
// a known file still fails; an entry whose count stops matching fails
// too (stale justifications are how drift hides). Parsing the AST (not
// the source text) means comments can never false-positive and gofmt
// line-wrapping can never hide a set.
func TestNoHandRolledTerminalSets(t *testing.T) {
	root := "../.." // pkg/store → repo root
	pkgs := []string{
		"pkg/store", "pkg/supervise", "pkg/runview",
		"pkg/runtime", "pkg/server/cloudpublisher",
		"pkg/cli", "pkg/notify", "pkg/worktreepool", "pkg/operatormcp", "pkg/runner",
	}

	statusNames := map[string]bool{
		"RunStatusFinished": true, "RunStatusFailed": true, "RunStatusFailedResumable": true,
		"RunStatusCancelled": true, "RunStatusPausedWaitingHuman": true, "RunStatusPausedOperator": true,
		"RunStatusRunning": true, "RunStatusQueued": true,
	}

	// statusesIn collects the distinct RunStatusX identifiers anywhere
	// under n (plain idents in pkg/store, selector idents elsewhere).
	statusesIn := func(n ast.Node) []string {
		set := map[string]bool{}
		ast.Inspect(n, func(c ast.Node) bool {
			if id, ok := c.(*ast.Ident); ok && statusNames[id.Name] {
				set[strings.TrimPrefix(id.Name, "RunStatus")] = true
			}
			return true
		})
		out := make([]string, 0, len(set))
		for k := range set {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}

	isLogical := func(e ast.Expr) bool {
		b, ok := e.(*ast.BinaryExpr)
		return ok && (b.Op == token.LAND || b.Op == token.LOR)
	}

	found := map[string]int{}
	for _, p := range pkgs {
		dir := filepath.Join(root, p)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", rel, perr)
			}
			record := func(n ast.Node) {
				combo := statusesIn(n)
				if len(combo) >= 2 {
					found[rel+" :: "+strings.Join(combo, "+")]++
				}
			}
			// Operand children of a logical chain are marked so only
			// the MAXIMAL &&/|| expression records (one set, not N).
			logicalChild := map[ast.Node]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CaseClause:
					// The clause's exprs as ONE grouped set — a
					// `case A, B, C:` is a single policy set however
					// it wraps.
					set := map[string]bool{}
					for _, e := range v.List {
						for _, s := range statusesIn(e) {
							set[s] = true
						}
					}
					if len(set) >= 2 {
						combo := make([]string, 0, len(set))
						for k := range set {
							combo = append(combo, k)
						}
						sort.Strings(combo)
						found[rel+" :: "+strings.Join(combo, "+")]++
					}
					return true // body still walked for nested sets
				case *ast.CompositeLit:
					record(v)
					return false // counted once, as this literal
				case *ast.CallExpr:
					// append(base, RunStatusX, RunStatusY, …) builds a
					// set without a composite literal.
					if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "append" {
						set := map[string]bool{}
						for _, a := range v.Args[1:] {
							for _, st := range statusesIn(a) {
								set[st] = true
							}
						}
						if len(set) >= 2 {
							combo := make([]string, 0, len(set))
							for k := range set {
								combo = append(combo, k)
							}
							sort.Strings(combo)
							found[rel+" :: "+strings.Join(combo, "+")]++
						}
					}
				case *ast.BinaryExpr:
					if isLogical(v) {
						if isLogical(v.X) {
							logicalChild[v.X] = true
						}
						if isLogical(v.Y) {
							logicalChild[v.Y] = true
						}
						if !logicalChild[n] {
							record(v)
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", p, err)
		}
	}

	for key, n := range found {
		if strings.HasPrefix(key, "pkg/store/lifecycle.go ::") || strings.HasPrefix(key, "pkg/store/run.go ::") {
			continue // the contract and the predicates beside the enum
		}
		e, ok := negativeSpaceAllowlist[key]
		if !ok {
			t.Errorf("hand-rolled RunStatus set: %q (×%d) — replace it with a lifecycle.go predicate or allowlist it with its reason", key, n)
			continue
		}
		if e.count != n {
			t.Errorf("allowlist entry %q expects %d occurrence(s), found %d — a set was added or removed; re-justify", key, e.count, n)
		}
	}
	for key, e := range negativeSpaceAllowlist {
		if _, ok := found[key]; !ok {
			t.Errorf("stale allowlist entry %q (%s) — the set is gone; prune the entry", key, e.reason)
		}
	}
}

type allowEntry struct {
	count  int
	reason string
}

// negativeSpaceAllowlist: every surviving multi-status set outside the
// contract, keyed "file :: combo", each with its occurrence count and
// the reason it is NOT a predicate call. Adding a set means arguing its
// reason here.
var negativeSpaceAllowlist = map[string]allowEntry{
	// -- pkg/store: transition machinery + the conformance harness.
	"pkg/store/store_run.go :: Cancelled+Failed+FailedResumable+Finished":                                                              {1, "applyStatusTransition side-effect switch (FinishedAt stamping)"},
	"pkg/store/store_run.go :: Finished+Running":                                                                                       {1, "applyStatusTransition checkpoint-clear pair"},
	"pkg/store/store_run.go :: PausedWaitingHuman+Running":                                                                             {1, "applyStatusTransition FinishedAt-clear pair"},
	"pkg/store/mongo/runs.go :: Cancelled+Failed+FailedResumable+Finished":                                                             {2, "runStatusUpdate side-effect switch + ListNotifiableRuns terminal $in (a bson filter cannot call a predicate; kept in lockstep with IsTerminal by the sweep contract)"},
	"pkg/store/mongo/runs.go :: Queued+Running":                                                                                        {1, "CountsAgainstLaunchLimit twin inside CountActiveRunsByTenant's $in filter"},
	"pkg/store/storetest/conformance.go :: Cancelled+Failed+FailedResumable+Finished+PausedOperator+PausedWaitingHuman+Queued+Running": {1, "tombstone canary passes every status to prove no CAS writes on a deleted run"},

	// -- pkg/runview: individually documented divergences.
	"pkg/runview/rewind.go :: Cancelled+Failed+FailedResumable+PausedOperator+PausedWaitingHuman+Queued": {1, "rewindableStatuses: deliberately wider than CanOperatorResume (failed stays rewindable, queued claimable)"},
	"pkg/runview/service_control.go :: FailedResumable+PausedOperator+PausedWaitingHuman":                {1, "CancelInactive flippable arm (queued has its own case; failed deliberately excluded; no running)"},
	"pkg/runview/service_control.go :: Cancelled+Failed+Finished":                                        {1, "inbox refuse set: failed_resumable deliberately ACCEPTED (drained on resume)"},
	"pkg/runview/service_lifecycle.go :: PausedWaitingHuman+Running":                                     {1, "sandboxContainerReapable keep-set: documented wider-than-IsTerminal reaping"},
	"pkg/runview/subbot.go :: Cancelled+Failed+FailedResumable":                                          {2, "subbot outcome routing (reattach switch + park-wait poll): failure-trio → clear + rerun fresh"},

	// -- pkg/supervise.
	"pkg/supervise/inproc.go :: Cancelled+Failed+Finished": {1, "steering inbox refuse set: failed_resumable deliberately accepted (drained on resume)"},

	// -- pkg/runtime: claim-CAS / routing sets (they gate a TRANSITION,
	// not external eligibility — see CanOperatorResume's doc).
	"pkg/runtime/run_failure.go :: FailedResumable+PausedOperator+PausedWaitingHuman+Running": {1, "engine ctx-cancel CAS: CanBeCancelled minus queued — a queued doc is a NEWER attempt this engine does not own"},
	"pkg/runtime/resume.go :: Cancelled+FailedResumable+PausedOperator":                       {1, "Resume dispatch: routes the failure-shaped statuses to resumeFromFailure (the answers path is a separate case)"},
	"pkg/runtime/resume.go :: Cancelled+FailedResumable+PausedOperator+Queued":                {1, "failure-resume claim: CanOperatorResume minus paused_waiting_human (answers path) plus queued (cloud pre-flip)"},
	"pkg/runtime/worktree.go :: Cancelled+Failed+Finished":                                    {1, "RecoverFinalize gate: only fully-stopped shapes finalize; failed_resumable waits for resume-or-cancel"},

	// -- pkg/cli.
	"pkg/cli/issue.go :: PausedOperator+PausedWaitingHuman+Queued+Running": {1, "--clear-last-run guard: refuse while any live-or-parked-on-human shape holds the pointer"},
	"pkg/cli/runs_prune.go :: Cancelled+Failed+FailedResumable+Finished":   {1, "prune name map: the statuses --status accepts by name"},
	"pkg/cli/runs_prune.go :: Cancelled+Failed+Finished":                   {1, "prune DEFAULT set: failed_resumable deliberately excluded (only explicit --status touches resumable work)"},

	// -- pkg/runner.
	"pkg/runner/loop.go :: Failed+Finished+PausedWaitingHuman": {1, "stale-delivery drop set: shapes a redelivery can never legitimately target"},
	"pkg/runner/loop.go :: FailedResumable+PausedOperator":     {1, "redelivery auto-convert-to-Resume pair (dispatcher-parked shapes)"},
	"pkg/runner/loop_nats.go :: Queued+Running":                {1, "DLQ park CAS: only a claimed-or-queued attempt may be parked"},
	"pkg/runner/usage_cap.go :: Queued+Running":                {1, "usage-cap park CAS: only a claimed-or-queued attempt may be parked"},

	// -- pkg/worktreepool.
	"pkg/worktreepool/classify.go :: PausedOperator+PausedWaitingHuman": {1, "isPausedResumable: the paused pair guarding checkout sparing (delegating would hide the GC policy nuance its doc carries)"},
}
