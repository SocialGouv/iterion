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
		"pkg/runtime", "pkg/server/cloudpublisher", "pkg/dispatcher",
		"pkg/cli", "pkg/notify", "pkg/worktreepool", "pkg/operatormcp", "pkg/runner",
	}

	statusNames := map[string]bool{
		"RunStatusFinished": true, "RunStatusFailed": true, "RunStatusFailedResumable": true,
		"RunStatusCancelled": true, "RunStatusPausedWaitingHuman": true, "RunStatusPausedOperator": true,
		"RunStatusRunning": true, "RunStatusQueued": true,
	}
	statusStrings := map[string]string{
		`"finished"`: "Finished", `"failed"`: "Failed", `"failed_resumable"`: "FailedResumable",
		`"cancelled"`: "Cancelled", `"paused_waiting_human"`: "PausedWaitingHuman",
		`"paused_operator"`: "PausedOperator", `"running"`: "Running", `"queued"`: "Queued",
	}

	// statusesIn collects the distinct statuses anywhere under n: the
	// RunStatusX identifiers (plain in pkg/store, selector elsewhere)
	// AND exact string literals of the status values — a []string /
	// metric-label set spells the same policy.
	statusesIn := func(n ast.Node) []string {
		set := map[string]bool{}
		ast.Inspect(n, func(c ast.Node) bool {
			switch v := c.(type) {
			case *ast.Ident:
				if statusNames[v.Name] {
					set[strings.TrimPrefix(v.Name, "RunStatus")] = true
				}
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					if name, ok := statusStrings[v.Value]; ok {
						set[name] = true
					}
				}
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

	unparen := func(e ast.Expr) ast.Expr {
		for {
			if p, ok := e.(*ast.ParenExpr); ok {
				e = p.X
				continue
			}
			return e
		}
	}
	isLogical := func(e ast.Expr) bool {
		b, ok := unparen(e).(*ast.BinaryExpr)
		return ok && (b.Op == token.LAND || b.Op == token.LOR)
	}

	found := map[string][]string{}
	for _, p := range pkgs {
		dir := filepath.Join(root, p)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
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
			// funcAt names the enclosing function of a position — the
			// allowlist anchor, so two same-combo sets in one file stay
			// individually accountable (a swap cannot hide behind a
			// count).
			funcAt := func(pos token.Pos) string {
				for _, d := range file.Decls {
					if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos <= fd.End() {
						return fd.Name.Name
					}
				}
				return "<pkg-level>"
			}
			record := func(n ast.Node) {
				combo := statusesIn(n)
				if len(combo) >= 2 {
					key := rel + " :: " + strings.Join(combo, "+")
					found[key] = append(found[key], funcAt(n.Pos()))
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
					// it wraps — and any logical expr in the list is
					// marked so the BinaryExpr pass does not count it
					// a second time.
					set := map[string]bool{}
					for _, e := range v.List {
						for _, s := range statusesIn(e) {
							set[s] = true
						}
						if isLogical(e) {
							markLogicalDescendants(unparen(e), logicalChild)
							logicalChild[unparen(e)] = true
						}
					}
					if len(set) >= 2 {
						combo := make([]string, 0, len(set))
						for k := range set {
							combo = append(combo, k)
						}
						sort.Strings(combo)
						key := rel + " :: " + strings.Join(combo, "+")
						found[key] = append(found[key], funcAt(v.Pos()))
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
							key := rel + " :: " + strings.Join(combo, "+")
							found[key] = append(found[key], funcAt(v.Pos()))
						}
					}
				case *ast.BinaryExpr:
					if v.Op == token.LAND || v.Op == token.LOR {
						if !logicalChild[n] {
							record(v)
							// Every logical expr under this maximal
							// chain — through parens AND call args
							// (f(a||b) && c) — is part of it, never a
							// second set.
							markLogicalDescendants(v, logicalChild)
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

	for key, anchors := range found {
		sort.Strings(anchors)
		if strings.HasPrefix(key, "pkg/store/lifecycle.go ::") || strings.HasPrefix(key, "pkg/store/run.go ::") {
			continue // the contract and the predicates beside the enum
		}
		e, ok := negativeSpaceAllowlist[key]
		if !ok {
			t.Errorf("hand-rolled RunStatus set: %q (in %v) — replace it with a lifecycle.go predicate or allowlist it with its reason", key, anchors)
			continue
		}
		want := append([]string(nil), e.anchors...)
		sort.Strings(want)
		if strings.Join(want, ",") != strings.Join(anchors, ",") {
			t.Errorf("allowlist entry %q expects anchors %v, found %v — a set moved, was added or removed; re-justify per site", key, want, anchors)
		}
	}
	for key, e := range negativeSpaceAllowlist {
		if _, ok := found[key]; !ok {
			t.Errorf("stale allowlist entry %q (%s) — the set is gone; prune the entry", key, e.reason)
		}
	}
}

type allowEntry struct {
	// anchors: the enclosing function of each justified occurrence.
	anchors []string
	reason  string
}

// markLogicalDescendants flags every &&/|| expression under n as part
// of an already-recorded chain.
func markLogicalDescendants(n ast.Node, marked map[ast.Node]bool) {
	ast.Inspect(n, func(c ast.Node) bool {
		if b, ok := c.(*ast.BinaryExpr); ok && (b.Op == token.LAND || b.Op == token.LOR) && c != n {
			marked[c] = true
		}
		return true
	})
}

// negativeSpaceAllowlist: every surviving multi-status set outside the
// contract, keyed "file :: combo", each with its occurrence count and
// the reason it is NOT a predicate call. Adding a set means arguing its
// reason here.
var negativeSpaceAllowlist = map[string]allowEntry{
	// -- pkg/store: transition machinery + harnesses.
	"pkg/store/store_run.go :: Cancelled+Failed+FailedResumable+Finished":                                                              {[]string{"applyStatusTransitionOutcome"}, "FinishedAt-stamping side-effect switch (renamed by the outcome-bookkeeping merge)"},
	"pkg/store/store_run.go :: PausedWaitingHuman+Running":                                                                             {[]string{"applyStatusTransitionOutcome"}, "FinishedAt-clear pair (resume paths un-freeze the duration ticker)"},
	"pkg/store/mongo/runs.go :: Cancelled+Failed+FailedResumable+Finished":                                                             {[]string{"ListNotifiableRuns", "SaveRun"}, "the notifiable-sweep terminal $in + SaveRun's terminal-arrival episode increment (a bson filter/pipeline cannot call a predicate; both are IsTerminal's set — the transition choke point itself now derives via predicates in statusTransitionSet)"},
	"pkg/store/mongo/route_decisions.go :: Failed+FailedResumable+Finished":                                                            {[]string{"ListRoutableRuns"}, "the router's sweep set: deliberately NARROWER than IsTerminal — a cancelled run is an operator's stop and is never routed (design property, pinned by the router's cancelled fixture)"},
	"pkg/store/store_route_decisions.go :: Failed+FailedResumable+Finished":                                                            {[]string{"ListRoutableRuns"}, "FS twin of the router's sweep set (same reason)"},
	"pkg/store/mongo/runs.go :: Queued+Running":                                                                                        {[]string{"CountActiveRunsByTenant", "SaveRun"}, "CountsAgainstLaunchLimit twins inside $in filters (a bson expression cannot call a predicate): the launch-limit count, and SaveRun's routing-policy first-write window"},
	"pkg/store/storetest/conformance.go :: Cancelled+Failed+FailedResumable+Finished+PausedOperator+PausedWaitingHuman+Queued+Running": {[]string{"testTombstoneRefusesWriters"}, "tombstone canary passes every status to prove no CAS writes on a deleted run"},

	// -- pkg/runview.
	"pkg/runview/rewind.go :: Cancelled+Failed+FailedResumable+PausedOperator+PausedWaitingHuman+Queued": {[]string{"<pkg-level>"}, "rewindableStatuses: deliberately wider than CanOperatorResume (failed stays rewindable, queued claimable)"},
	"pkg/runview/service_control.go :: FailedResumable+PausedOperator+PausedWaitingHuman":                {[]string{"CancelInactiveCtx"}, "flippable arm: queued has its own case, failed deliberately excluded, no running"},
	"pkg/runview/service_control.go :: Cancelled+Failed+Finished":                                        {[]string{"QueueMessage"}, "inbox refuse set: failed_resumable deliberately ACCEPTED (drained on resume)"},
	"pkg/runview/service_lifecycle.go :: PausedWaitingHuman+Running":                                     {[]string{"sandboxContainerReapable"}, "keep-set: documented wider-than-IsTerminal reaping (paused_operator reapable)"},
	"pkg/runview/subbot.go :: Cancelled+Failed+FailedResumable":                                          {[]string{"AwaitSubbotTerminal", "ReattachSubbotChild"}, "subbot outcome routing: failure-trio → clear + rerun fresh"},

	// -- pkg/supervise.
	"pkg/supervise/inproc.go :: Cancelled+Failed+Finished": {[]string{"Inject"}, "steering inbox refuse set: failed_resumable deliberately accepted"},

	// -- pkg/runtime: claim-CAS / routing sets (transition gates, not
	// external eligibility — see CanOperatorResume's doc).
	"pkg/runtime/run_failure.go :: FailedResumable+PausedOperator+PausedWaitingHuman+Running": {[]string{"handleContextDoneWithCheckpoint"}, "engine ctx-cancel CAS: CanBeCancelled minus queued — a queued doc is a NEWER attempt this engine does not own"},
	"pkg/runtime/resume.go :: Cancelled+FailedResumable+PausedOperator":                       {[]string{"Resume"}, "Resume dispatch: failure-shaped statuses route to resumeFromFailure"},
	"pkg/runtime/resume.go :: Cancelled+FailedResumable+PausedOperator+Queued":                {[]string{"claimForFailureResume"}, "failure-resume claim: CanOperatorResume minus the answers path plus queued (cloud pre-flip)"},
	"pkg/runtime/worktree.go :: Cancelled+Failed+Finished":                                    {[]string{"RecoverFinalize"}, "finalize gate: only fully-stopped shapes; failed_resumable waits for resume-or-cancel"},

	// -- pkg/dispatcher: its own admission policies (the documented
	// CanAutoResume divergence lives HERE — enforced, not just written).
	"pkg/dispatcher/retry.go :: Cancelled+FailedResumable+PausedOperator+PausedWaitingHuman+Queued+Running": {[]string{"lastRunForbidsFresh"}, "everything but hard `failed` forbids a fresh sibling — the anti-double-launch hold"},
	"pkg/dispatcher/retry.go :: FailedResumable+PausedOperator":                                             {[]string{"resumableRunID"}, "the CanAutoResume DIVERGENCE named in lifecycle.go: a dispatcher-owned paused_operator is machinery state, re-dispatched"},
	"pkg/dispatcher/loop.go :: FailedResumable+PausedOperator":                                              {[]string{"resolveRunID"}, "retry-entry adoption pair (mirrors resumableRunID)"},
	"pkg/dispatcher/loop.go :: PausedOperator+PausedWaitingHuman":                                           {[]string{"lastRunHoldBeforeClaim"}, "dispatcher-paused re-dispatch arm"},
	"pkg/dispatcher/loop.go :: Queued+Running":                                                              {[]string{"lastRunHoldBeforeClaim"}, "live-run hold arm"},
	"pkg/dispatcher/parked.go :: PausedOperator+PausedWaitingHuman":                                         {[]string{"isDispatcherPausedRun"}, "IsPaused twin + dispatcher-source check (kept literal beside its source predicate)"},

	// -- pkg/cli.
	"pkg/cli/issue.go :: PausedOperator+PausedWaitingHuman+Queued+Running": {[]string{"refuseClearWhileRunAlive"}, "--clear-last-run guard: refuse while any live-or-parked-on-human shape holds the pointer"},
	"pkg/cli/runs_prune.go :: Cancelled+Failed+FailedResumable+Finished":   {[]string{"<pkg-level>"}, "prune name map: the statuses --status accepts"},
	"pkg/cli/runs_prune.go :: Cancelled+Failed+Finished":                   {[]string{"validatePruneStatuses"}, "prune DEFAULT set: failed_resumable deliberately excluded"},
	"pkg/cli/remote_runs.go :: Cancelled+Failed+FailedResumable+Finished":  {[]string{"followRemoteRun"}, "--follow stop set over the WIRE statuses (strings): IsTerminal's set; the paused non-exit is the known bug #3 follow-up card"},

	// -- pkg/runner.
	"pkg/runner/loop.go :: Failed+Finished":                     {[]string{"bankableStatus"}, "forge-banking outcomes (finalStatus strings; budget_exceeded rides along outside the run-status vocabulary)"},
	"pkg/runner/loop.go :: Failed+Finished+PausedWaitingHuman":  {[]string{"resolveDeliveryPreconditions"}, "stale-delivery drop set: shapes a redelivery can never legitimately target"},
	"pkg/runner/loop.go :: FailedResumable+PausedOperator":      {[]string{"resolveDeliveryPreconditions"}, "redelivery auto-convert-to-Resume pair (dispatcher-parked shapes)"},
	"pkg/runner/loop_nats.go :: FailedResumable+Queued+Running": {[]string{"parkOnDLQOnFinalDelivery"}, "DLQ park CAS: claimed/queued attempts AND the engine's own failed_resumable write — on the nominal path the engine parks first, and a CAS that excluded it dropped the DLQ_PARKED cause silently (gate F4)"},
	"pkg/runner/usage_cap.go :: Queued+Running":                 {[]string{"usageCapPreflight"}, "usage-cap park CAS: only a claimed-or-queued attempt may be parked"},

	// -- pkg/worktreepool.
	"pkg/worktreepool/classify.go :: PausedOperator+PausedWaitingHuman": {[]string{"isPausedResumable"}, "the paused pair guarding checkout sparing (GC policy nuance documented at the site)"},
}
