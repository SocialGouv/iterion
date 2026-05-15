package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// securedRenovacyStubInputs maps every var that the workflow's edges
// template against `vars.X`. The bot declares no top-level `input:`
// schema — inputs flow via `vars:` defaults that this map overrides.
// `workspace_dir` must be set or every edge templating
// `{{vars.workspace_dir}}` resolves to the empty string and downstream
// tools that need a real path silently no-op (we never reach the tools
// here, but keeping the value realistic surfaces clearer errors).
var securedRenovacyStubInputs = map[string]interface{}{
	"workspace_dir":        "/tmp/sr-stub",
	"user_prompt":          "",
	"override_install_cmd": "",
	"override_upgrade_cmd": "",
	"scope":                "patch",
	"max_packages_per_run": 1,
	"major_policy":         "skip",
	"fix_loop_default":     1,
	"fix_loop_major":       1,
	"update_scope":         "libraries",
}

// stackProfileStub returns a minimal detect_stack output that satisfies
// the stack_profile schema (ecosystems json, primary_ecosystem_id, plus
// the legacy primary-mirror fields). Used by every secured-renovacy
// test as the head session for every downstream edge templating
// `{{outputs.detect_stack.X}}`.
func stackProfileStub() map[string]interface{} {
	return map[string]interface{}{
		"ecosystems": []interface{}{
			map[string]interface{}{
				"id":          "yarn",
				"pkg_manager": "yarn",
				"repo_kind":   "single-package",
				"workspaces":  []interface{}{},
				"upgrade_cmd": "yarn up",
				"install_cmd": "yarn install",
				"lock_files":  []interface{}{"yarn.lock"},
				"manifests":   []interface{}{"package.json"},
				"notes":       "",
			},
		},
		"primary_ecosystem_id": "yarn",
		"pkg_manager":          "yarn",
		"repo_kind":            "single-package",
		"workspaces":           []interface{}{},
		"upgrade_cmd":          "yarn up",
		"install_cmd":          "yarn install",
		"lock_files":           []interface{}{"yarn.lock"},
		"notes":                "",
		"_session_id":          "sess-detect-1",
		"_session_fingerprint": "fp-detect-1",
	}
}

// onceTrueThenFalse returns a closure that yields true on its first
// call and false thereafter. Used to make select_candidate return
// `has_more:true` for one iteration (one package to process) then
// `has_more:false` so the loop exits.
func onceTrueThenFalse() func() bool {
	calls := 0
	return func() bool {
		calls++
		return calls == 1
	}
}

// TestSecuredRenovacy_PatchFastTrack covers the cheapest successful
// path through the workflow:
//
//	detect_stack → capture_start_sha → discover_outdated (1 patch) →
//	bucket_patches(has_patches:true) → batch_upgrade_patches →
//	batch_commit(committed:true) → write_audit_md(was_batch:true) →
//	bucket_families(has_families:false) → select_candidate(has_more:false) →
//	phase2_decider(go_done:true via only_patches_attempted) →
//	emit_sbom → done
//
// Asserts: emit_sbom ran once, no per-package nodes touched, no
// reviewer ran (phase2_decider short-circuits to done).
func TestSecuredRenovacy_PatchFastTrack(t *testing.T) {
	wf := compileFixtureStubSafe(t, "secured-renovacy/bot.bot")
	exec := newScenarioExecutor()

	exec.on("detect_stack", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return stackProfileStub(), nil
	})
	exec.on("capture_start_sha", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"sha": "deadbeef0000000000000000000000000000beef"}, nil
	})
	exec.on("discover_outdated", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"packages": []interface{}{
				map[string]interface{}{
					"name":      "left-pad",
					"current":   "1.0.0",
					"target":    "1.0.1",
					"risk":      "patch",
					"ecosystem": "yarn",
					"kind":      "library",
					"dep_type":  "runtime",
					"workspace": "",
				},
			},
			"count":                1,
			"raw":                  "left-pad 1.0.0 → 1.0.1",
			"per_ecosystem_counts": map[string]interface{}{"yarn": 1},
		}, nil
	})
	exec.on("bucket_patches", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"patches": []interface{}{
				map[string]interface{}{"name": "left-pad", "current": "1.0.0", "target": "1.0.1"},
			},
			"has_patches":           true,
			"patches_count":         1,
			"attempted_after_batch": map[string]interface{}{"left-pad": "1.0.1"},
		}, nil
	})
	exec.on("batch_upgrade_patches", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success":        true,
			"output":         "left-pad 1.0.0 → 1.0.1",
			"upgraded_specs": []interface{}{"left-pad@1.0.1"},
		}, nil
	})
	exec.on("batch_commit", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success":   true,
			"committed": true,
			"output":    "committed abc1234",
			"sha":       "abc1234567890123456789012345678901234567",
		}, nil
	})
	exec.on("write_audit_md", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success":   true,
			"output":    "audit md amended into abc1234 (left-pad-1.0.0-to-1.0.1.md)",
			"was_batch": true,
		}, nil
	})
	exec.on("bucket_families", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"families":       []interface{}{},
			"has_families":   false,
			"families_count": 0,
			"solos":          []interface{}{},
			"solos_count":    0,
		}, nil
	})
	exec.on("select_candidate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Patch fast-track path → arrive here with attempted ledger non-empty
		// and no remaining survivors → has_more:false, only_patches:true.
		return map[string]interface{}{
			"selected_package":       "",
			"current_version":        "",
			"target_version":         "",
			"risk":                   "patch",
			"has_more":               false,
			"attempted_count":        1,
			"cumulative_attempted":   map[string]interface{}{"left-pad": "1.0.1"},
			"fix_loop_max":           1,
			"ecosystem":              "yarn",
			"kind":                   "library",
			"dep_type":               "runtime",
			"workspace":              "",
			"only_patches_attempted": true,
			"remaining_count":        0,
			"cap_reason":             "exhausted",
		}, nil
	})
	exec.on("emit_sbom", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success": true,
			"path":    "docs/renovacy/sbom-abc1234.json",
			"count":   1,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-sr-patch", securedRenovacyStubInputs); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-sr-patch")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if exec.callCount("emit_sbom") != 1 {
		t.Errorf("expected emit_sbom once, got %d", exec.callCount("emit_sbom"))
	}
	if exec.wasCalled("reviewer_claude") || exec.wasCalled("reviewer_gpt") {
		t.Errorf("phase2 reviewers should NOT run when only_patches_attempted=true")
	}
	if exec.wasCalled("upgrade") || exec.wasCalled("install") {
		t.Errorf("per-package nodes should NOT run on patch fast-track")
	}
	// Note: phase2_decider, intel_join, streak_check, join_files,
	// mark_failed_and_continue are `compute` nodes — evaluated by the
	// runtime's expression engine, NOT routed through NodeExecutor.
	// They never show up in scenarioExecutor.calls; assertions cover
	// only nodes that actually go through Execute().
	for _, gate := range []string{"detect_stack", "capture_start_sha", "discover_outdated",
		"bucket_patches", "batch_upgrade_patches", "batch_commit", "write_audit_md",
		"bucket_families", "select_candidate", "emit_sbom"} {
		if !exec.wasCalled(gate) {
			t.Errorf("expected node %q to be called, was not", gate)
		}
	}
}

// TestSecuredRenovacy_PerPackageMinor covers the per-package solo
// path with a single minor candidate. Exercises every per-package node
// in sequence and reaches Phase 2 cross-family review:
//
//	detect_stack → capture_start_sha → discover_outdated(1 minor) →
//	bucket_patches(has_patches:false) → select_candidate(has_more:true once) →
//	resolve_pkg_ecosystem → intel_fanout → security_audit + changelog_review →
//	intel_join(safe:true) → upgrade(success:true) → install(success:true) →
//	align_code → validate_upgrade(stable:true) → prepare_commit → join_files →
//	commit_changes → write_audit_md(was_batch:false) →
//	select_candidate(has_more:false) → phase2_decider(go_done:false) →
//	alt_review → reviewer_claude(approve) → streak_check → alt_review →
//	reviewer_gpt(approve) → streak_check.stop → emit_sbom → done
func TestSecuredRenovacy_PerPackageMinor(t *testing.T) {
	wf := compileFixtureStubSafe(t, "secured-renovacy/bot.bot")
	exec := newScenarioExecutor()

	exec.on("detect_stack", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return stackProfileStub(), nil
	})
	exec.on("capture_start_sha", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"sha": "deadbeef0000000000000000000000000000beef"}, nil
	})
	exec.on("discover_outdated", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"packages": []interface{}{
				map[string]interface{}{
					"name": "react", "current": "18.2.0", "target": "18.3.0",
					"risk": "minor", "ecosystem": "yarn",
					"kind": "library", "dep_type": "runtime", "workspace": "",
				},
			},
			"count":                1,
			"raw":                  "react 18.2.0 → 18.3.0",
			"per_ecosystem_counts": map[string]interface{}{"yarn": 1},
		}, nil
	})
	exec.on("bucket_patches", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"patches":               []interface{}{},
			"has_patches":           false,
			"patches_count":         0,
			"attempted_after_batch": map[string]interface{}{},
		}, nil
	})

	hasMore := onceTrueThenFalse()
	exec.on("select_candidate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		more := hasMore()
		if more {
			return map[string]interface{}{
				"selected_package":       "react",
				"current_version":        "18.2.0",
				"target_version":         "18.3.0",
				"risk":                   "minor",
				"has_more":               true,
				"attempted_count":        0,
				"cumulative_attempted":   map[string]interface{}{},
				"fix_loop_max":           3,
				"ecosystem":              "yarn",
				"kind":                   "library",
				"dep_type":               "runtime",
				"workspace":              "",
				"only_patches_attempted": false,
				"remaining_count":        1,
				"cap_reason":             "",
			}, nil
		}
		return map[string]interface{}{
			"selected_package":       "",
			"has_more":               false,
			"attempted_count":        1,
			"cumulative_attempted":   map[string]interface{}{"react": "18.3.0"},
			"only_patches_attempted": false,
			"remaining_count":        0,
			"cap_reason":             "exhausted",
		}, nil
	})
	exec.on("resolve_pkg_ecosystem", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"pkg_manager": "yarn", "install_cmd": "yarn install", "upgrade_cmd": "yarn up",
			"workspaces": []interface{}{}, "lock_files": []interface{}{"yarn.lock"},
			"notes": "", "resolved_id": "yarn", "matched": true,
		}, nil
	})
	exec.on("security_audit", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"safe":               true,
			"cves":               []interface{}{},
			"malware_signals":    []interface{}{},
			"source":             "osv-scanner",
			"raw":                "no advisories",
			"blockers":           []interface{}{},
			"fix_plan":           "",
			"advisory_chains":    []interface{}{},
			"auditors_consulted": []interface{}{"osv-scanner"},
			"_session_id":        "sess-audit-1",
		}, nil
	})
	exec.on("changelog_review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"has_breaking":               false,
			"breaking_changes":           []interface{}{},
			"alignment_steps":            []interface{}{},
			"references":                 []interface{}{"https://github.com/facebook/react/releases/tag/v18.3.0"},
			"confidence":                 "high",
			"peer_dependency_changes":    []interface{}{},
			"engine_requirement_changes": []interface{}{},
			"_session_id":                "sess-changelog-1",
		}, nil
	})
	exec.on("upgrade", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "output": "upgraded react"}, nil
	})
	exec.on("install", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "output": "installed"}, nil
	})
	exec.on("align_code", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success": true, "summary": "no consuming code changes needed",
			"_session_id": "sess-align-1",
		}, nil
	})
	exec.on("validate_upgrade", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"stable":       true,
			"blockers":     []interface{}{},
			"fix_plan":     "",
			"confidence":   "high",
			"commands_run": []interface{}{"yarn typecheck", "yarn test"},
		}, nil
	})
	exec.on("prepare_commit", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"type":         "chore",
			"scope":        "deps",
			"subject":      "react 18.2.0 → 18.3.0",
			"full_message": "chore(deps): react 18.2.0 → 18.3.0",
			"files":        []interface{}{"package.json", "yarn.lock"},
			"committed":    false,
		}, nil
	})
	exec.on("commit_changes", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success": true,
			"output":  "committed abc1234",
			"sha":     "abc1234567890123456789012345678901234567",
		}, nil
	})
	exec.on("write_audit_md", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success":   true,
			"output":    "audit md amended into abc1234 (react-18.2.0-to-18.3.0.md)",
			"was_batch": false,
		}, nil
	})
	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("claude"), nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("gpt"), nil
	})
	exec.on("emit_sbom", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "path": "docs/renovacy/sbom-abc.json", "count": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-sr-minor", securedRenovacyStubInputs); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-sr-minor")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	for _, gate := range []string{"upgrade", "install", "align_code", "validate_upgrade",
		"prepare_commit", "commit_changes", "reviewer_claude", "reviewer_gpt", "emit_sbom"} {
		if !exec.wasCalled(gate) {
			t.Errorf("expected per-package node %q to be called, was not", gate)
		}
	}
	if exec.wasCalled("fix_after_upgrade") {
		t.Errorf("fix_after_upgrade should NOT run when validate_upgrade is stable on first try")
	}
}

// TestSecuredRenovacy_FixLoopThenCommit drives one cycle of the
// per-package fix loop: validate_upgrade returns stable=false on the
// first invocation, fix_after_upgrade applies, validate_upgrade
// returns stable=true on the second invocation. The run then proceeds
// through commit and exits via the phase2 reviewers.
func TestSecuredRenovacy_FixLoopThenCommit(t *testing.T) {
	wf := compileFixtureStubSafe(t, "secured-renovacy/bot.bot")
	exec := newScenarioExecutor()

	// Reuse all per-package stubs from PerPackageMinor.
	exec.on("detect_stack", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return stackProfileStub(), nil
	})
	exec.on("capture_start_sha", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"sha": "deadbeef0000000000000000000000000000beef"}, nil
	})
	exec.on("discover_outdated", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"packages": []interface{}{
				map[string]interface{}{
					"name": "react", "current": "18.2.0", "target": "18.3.0",
					"risk": "minor", "ecosystem": "yarn",
					"kind": "library", "dep_type": "runtime", "workspace": "",
				},
			},
			"count":                1,
			"raw":                  "react minor",
			"per_ecosystem_counts": map[string]interface{}{"yarn": 1},
		}, nil
	})
	exec.on("bucket_patches", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"patches": []interface{}{}, "has_patches": false,
			"patches_count": 0, "attempted_after_batch": map[string]interface{}{},
		}, nil
	})
	hasMore := onceTrueThenFalse()
	exec.on("select_candidate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		if hasMore() {
			return map[string]interface{}{
				"selected_package": "react", "current_version": "18.2.0", "target_version": "18.3.0",
				"risk": "minor", "has_more": true, "attempted_count": 0,
				"cumulative_attempted": map[string]interface{}{},
				"fix_loop_max":         3, "ecosystem": "yarn", "kind": "library",
				"dep_type": "runtime", "workspace": "",
				"only_patches_attempted": false, "remaining_count": 1, "cap_reason": "",
			}, nil
		}
		return map[string]interface{}{
			"selected_package": "", "has_more": false, "attempted_count": 1,
			"cumulative_attempted":   map[string]interface{}{"react": "18.3.0"},
			"only_patches_attempted": false, "remaining_count": 0, "cap_reason": "exhausted",
		}, nil
	})
	exec.on("resolve_pkg_ecosystem", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"pkg_manager": "yarn", "install_cmd": "yarn install", "upgrade_cmd": "yarn up",
			"workspaces": []interface{}{}, "lock_files": []interface{}{"yarn.lock"},
			"notes": "", "resolved_id": "yarn", "matched": true,
		}, nil
	})
	exec.on("security_audit", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"safe": true, "cves": []interface{}{}, "malware_signals": []interface{}{},
			"source": "osv-scanner", "raw": "ok", "blockers": []interface{}{},
			"fix_plan": "", "advisory_chains": []interface{}{},
			"auditors_consulted": []interface{}{"osv-scanner"}, "_session_id": "sess-audit-1",
		}, nil
	})
	exec.on("changelog_review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"has_breaking": false, "breaking_changes": []interface{}{},
			"alignment_steps": []interface{}{}, "references": []interface{}{},
			"confidence":                 "high",
			"peer_dependency_changes":    []interface{}{},
			"engine_requirement_changes": []interface{}{},
			"_session_id":                "sess-changelog-1",
		}, nil
	})
	exec.on("upgrade", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "output": "upgraded"}, nil
	})
	exec.on("install", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "output": "installed"}, nil
	})
	exec.on("align_code", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "summary": "aligned", "_session_id": "sess-align-1"}, nil
	})

	// validate_upgrade: first call → unstable (drives fix_loop);
	//                   second call → stable (commit lands).
	validateCalls := 0
	exec.on("validate_upgrade", func(_ map[string]interface{}) (map[string]interface{}, error) {
		validateCalls++
		if validateCalls == 1 {
			return map[string]interface{}{
				"stable":       false,
				"blockers":     []interface{}{"typecheck failed in src/App.tsx"},
				"fix_plan":     "update prop types for React 18.3 changes",
				"confidence":   "high",
				"commands_run": []interface{}{"yarn typecheck"},
			}, nil
		}
		return map[string]interface{}{
			"stable":       true,
			"blockers":     []interface{}{},
			"fix_plan":     "",
			"confidence":   "high",
			"commands_run": []interface{}{"yarn typecheck", "yarn test"},
		}, nil
	})
	exec.on("fix_after_upgrade", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied":       true,
			"summary":       "fixed prop types in src/App.tsx",
			"files_changed": []interface{}{"src/App.tsx"},
			"_session_id":   "sess-fix-after-1",
		}, nil
	})
	exec.on("prepare_commit", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"type": "chore", "scope": "deps", "subject": "react 18.2.0 → 18.3.0",
			"full_message": "chore(deps): react 18.2.0 → 18.3.0",
			"files":        []interface{}{"package.json", "yarn.lock", "src/App.tsx"},
			"committed":    false,
		}, nil
	})
	exec.on("commit_changes", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success": true, "output": "committed", "sha": "abc1234",
		}, nil
	})
	exec.on("write_audit_md", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "output": "amended", "was_batch": false}, nil
	})
	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("claude"), nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("gpt"), nil
	})
	exec.on("emit_sbom", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "path": "docs/renovacy/sbom.json", "count": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-sr-fix", securedRenovacyStubInputs); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-sr-fix")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if exec.callCount("fix_after_upgrade") != 1 {
		t.Errorf("expected fix_after_upgrade once, got %d", exec.callCount("fix_after_upgrade"))
	}
	if exec.callCount("validate_upgrade") < 2 {
		t.Errorf("expected validate_upgrade ≥ 2 (failure + retry), got %d", exec.callCount("validate_upgrade"))
	}
	if exec.callCount("commit_changes") < 1 {
		t.Errorf("expected commit_changes ≥ 1, got %d", exec.callCount("commit_changes"))
	}
}
