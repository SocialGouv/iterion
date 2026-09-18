package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// secured-renovacy's loops hold their exhaustion exits (C145, #1293): the
// bare edge beside the loop edge, taken once the back-edge is declined —
// by the affordability guard, since both caps sit one above the operator's
// knob and select_candidate's own exit fires first at the cap.
func TestSecuredRenovacyExhaustionExitsShape(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "secured-renovacy/main.bot")

	plain := func(e *ir.Edge) bool {
		return e.Condition == "" && e.Expression == nil && !e.IsElse && e.LoopName == "" && e.ForeachName == ""
	}
	exit := func(from, to string) *ir.Edge {
		t.Helper()
		var found *ir.Edge
		for _, e := range wf.Edges {
			if e.From == from && e.To == to && plain(e) {
				if found != nil {
					t.Fatalf("two plain edges %s -> %s", from, to)
				}
				found = e
			}
		}
		if found == nil {
			t.Fatalf("no plain edge %s -> %s: the loop's exhaustion exit is missing", from, to)
		}
		return found
	}
	mapping := func(e *ir.Edge, key string) string {
		for _, m := range e.With {
			if m.Key == key {
				return m.Raw
			}
		}
		return ""
	}

	for _, loop := range []string{"package_loop", "family_loop"} {
		l := wf.Loops[loop]
		if l == nil {
			t.Fatalf("loop %s missing", loop)
		}
		if l.MaxIterationsExpr != "vars.max_packages_per_run + 1" {
			t.Errorf("%s cap = %q (literal %d), want the operator's knob plus one so select_candidate's own exit reports the cap first", loop, l.MaxIterationsExpr, l.MaxIterations)
		}
	}

	// The committed solo: the pick is counted in before the decider reads it.
	banked := exit("write_audit_md", "solo_banked")
	if got := mapping(banked, "attempted_count"); got != "{{outputs.select_candidate.attempted_count}}" {
		t.Errorf("write_audit_md -> solo_banked attempted_count = %q", got)
	}
	decided := exit("solo_banked", "phase2_decider")
	if got := mapping(decided, "attempted_count"); got != "{{outputs.solo_banked.attempted_count}}" {
		t.Errorf("solo_banked -> phase2_decider attempted_count = %q, want the banked count", got)
	}
	if got := mapping(decided, "only_patches_attempted"); got != "{{outputs.solo_banked.only_patches_attempted}}" {
		t.Errorf("solo_banked -> phase2_decider only_patches_attempted = %q", got)
	}
	// The failed solo: the pick was reverted, the ledger before it is right.
	failed := exit("mark_failed_and_continue", "phase2_decider")
	if got := mapping(failed, "attempted_count"); got != "{{outputs.select_candidate.attempted_count}}" {
		t.Errorf("mark_failed_and_continue -> phase2_decider attempted_count = %q", got)
	}
	// The family loop hands what is left to the solo loop with the fresh ledger.
	family := exit("mark_family_attempted", "select_candidate")
	if got := mapping(family, "attempted"); got != "{{outputs.mark_family_attempted.attempted_members}}" {
		t.Errorf("mark_family_attempted -> select_candidate attempted = %q, want the ledger mark_family_attempted just grew", got)
	}
	for _, key := range []string{"packages", "scope", "max_packages", "workspace_dir", "update_scope"} {
		if mapping(family, key) == "" {
			t.Errorf("mark_family_attempted -> select_candidate lost the %q mapping", key)
		}
	}
}

// The defect the gate found on the first draft of the solo exit: on the
// budget path the exit is reached at ANY iteration, and select_candidate's
// attempted_count is the ledger BEFORE the pick it handed out — 0 for the
// first solo — so phase2_decider sent a committed minor upgrade straight
// to emit_sbom and done. Driven on the real engine with a stub executor:
// one minor upgrade committed, then the guard declines the back-edge
// (one more iteration would cross the 90% hard threshold), and the run
// must reach Phase 2's campaign, never the SBOM.
func TestSecuredRenovacyBudgetDeclinedSoloGoesToPhase2(t *testing.T) {
	t.Parallel()
	wf := compileFixtureStubSafe(t, "secured-renovacy/main.bot")
	wf.Worktree = "none"
	wf.Budget.MaxCostUSD = 1.0

	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	exec := newScenarioExecutor()
	priced := func(usd float64, out map[string]any) func(map[string]any) (map[string]any, error) {
		return func(map[string]any) (map[string]any, error) {
			o := map[string]any{"_tokens": 100, "_cost_usd": usd}
			for k, v := range out {
				o[k] = v
			}
			return o, nil
		}
	}
	// Before the loop's body: $0.10.
	exec.on("detect_stack", priced(0.05, map[string]any{
		"ecosystems":           []any{map[string]any{"id": "npm", "pkg_manager": "npm"}},
		"primary_ecosystem_id": "npm", "pkg_manager": "npm", "repo_kind": "single",
		"workspaces": []any{}, "upgrade_cmd": "npm install", "install_cmd": "npm ci",
		"lock_files": []any{"package-lock.json"}, "manifests": []any{"package.json"}, "notes": "",
	}))
	exec.on("capture_start_sha", priced(0, map[string]any{"sha": "0000000"}))
	exec.on("discover_outdated", priced(0.05, map[string]any{
		"packages": []any{map[string]any{"name": "debug", "current": "4.3.0", "target": "4.4.3", "risk": "minor", "kind": "libraries", "ecosystem": "npm"}},
		"count":    1, "per_ecosystem_counts": map[string]any{"npm": 1},
	}))
	exec.on("bucket_patches", priced(0, map[string]any{"has_patches": false, "patches": []any{}, "attempted_after_batch": map[string]any{}}))
	// The first solo pick: the count BEFORE it is 0, the ledger includes it.
	exec.on("select_candidate", priced(0, map[string]any{
		"selected_package": "debug", "current_version": "4.3.0", "target_version": "4.4.3", "risk": "minor",
		"has_more": true, "attempted_count": 0,
		"cumulative_attempted":   map[string]any{"debug": map[string]any{"current": "4.3.0", "target": "4.4.3", "risk": "minor"}},
		"fix_loop_max":           3,
		"only_patches_attempted": false, "remaining_count": 0, "cap_reason": "",
		"ecosystem": "npm", "kind": "libraries", "dep_type": "", "workspace": "",
	}))
	exec.on("resolve_pkg_ecosystem", priced(0, map[string]any{
		"pkg_manager": "npm", "install_cmd": "npm ci", "upgrade_cmd": "npm install",
		"workspaces": []any{}, "lock_files": []any{"package-lock.json"}, "notes": "",
	}))
	// The body: $0.45 in all, so used + the next iteration's price reaches 90% of $1.
	exec.on("security_audit", priced(0.10, map[string]any{"safe": true, "blockers": []any{}, "fix_plan": "", "advisory_chains": []any{}, "cves": []any{}, "malware_signals": []any{}, "source": "stub", "raw": ""}))
	exec.on("changelog_review", priced(0.10, map[string]any{"has_breaking": false, "breaking_changes": []any{}, "alignment_steps": []any{}, "references": []any{}, "confidence": "high", "peer_dependency_changes": []any{}, "engine_requirement_changes": []any{}}))
	exec.on("upgrade", priced(0.10, map[string]any{"success": true, "exit_code": 0, "output": ""}))
	exec.on("install", priced(0.05, map[string]any{"success": true, "exit_code": 0, "output": ""}))
	exec.on("align_code", priced(0.05, map[string]any{"applied": false, "summary": "no alignment needed", "files_changed": []any{}}))
	exec.on("validate_upgrade", priced(0.05, map[string]any{"stable": true, "blockers": []any{}, "fix_plan": "", "confidence": "high", "commands_run": []any{"npm test"}}))
	exec.on("prepare_commit", priced(0, map[string]any{"type": "chore", "scope": "deps", "subject": "update debug to 4.4.3", "full_message": "chore(deps): update debug to 4.4.3", "files": []any{"package.json", "package-lock.json"}, "committed": false}))
	exec.on("commit_changes", priced(0, map[string]any{"success": true, "output": "committed 1111111", "sha": "1111111"}))
	exec.on("write_audit_md", priced(0, map[string]any{"success": true, "output": "audit md written", "was_batch": false}))
	// The two terminals the decider can pick: reaching either ends the drive.
	exec.on("p2_campaign", func(map[string]any) (map[string]any, error) {
		return nil, errors.New("drive stopped at p2_campaign")
	})
	exec.on("emit_sbom", func(map[string]any) (map[string]any, error) {
		return nil, errors.New("drive stopped at emit_sbom")
	})

	eng := runtime.New(wf, s, exec)
	const runID = "e2e-renovacy-budget-declined-solo"
	err = eng.Run(context.Background(), runID, map[string]any{
		"scope": "patch,minor", "max_packages_per_run": 2, "update_scope": "libraries",
	})
	if err == nil {
		t.Fatal("the drive ran to completion; one of the two terminal stubs should have stopped it")
	}
	if got := exec.callCount("select_candidate"); got != 1 {
		t.Fatalf("select_candidate ran %d times, want 1 — the back-edge was not declined by the budget guard (run error: %v)", got, err)
	}
	if got := exec.callCount("emit_sbom"); got != 0 {
		t.Errorf("emit_sbom ran %d times: a committed minor upgrade went to done without Phase 2 (run error: %v)", got, err)
	}
	if got := exec.callCount("p2_campaign"); got != 1 {
		t.Errorf("p2_campaign ran %d times, want 1 — the banked upgrade must be reviewed (run error: %v)", got, err)
	}
	if !strings.Contains(err.Error(), "drive stopped at p2_campaign") {
		t.Errorf("run error = %v, want the drive stopped at p2_campaign", err)
	}

	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var declined bool
	var exitTo string
	for _, e := range events {
		if string(e.Type) == "budget_warning" && e.Data["reason"] == "loop_budget_guard" && e.Data["loop"] == "package_loop" {
			declined = true
		}
		if string(e.Type) == "edge_selected" && e.Data["from"] == "write_audit_md" {
			exitTo, _ = e.Data["to"].(string)
		}
	}
	if !declined {
		t.Error("no budget_warning with reason loop_budget_guard for package_loop: the exit was reached some other way")
	}
	if exitTo != "solo_banked" {
		t.Errorf("write_audit_md left through %q, want solo_banked (the exit that counts the banked pick)", exitTo)
	}
}
