package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/testparallel"
)

// The t.Parallel campaign of this package, and the reason the rest of it is
// serial — ONE place, so nobody has to reconstruct it (#2789, ARB-58 = A).
//
// This package was the next floor of the CI `race` leg — 1051 s, read on
// merge_group run 39133360570 — with ZERO t.Parallel() in 214 test files. The
// campaign made the 138 files that need nothing from production code run in
// parallel, and DECLARES the other 75 serial: they read or write the process
// environment through t.Setenv, or install a package global. t.Setenv and
// t.Parallel exclude each other — t.Setenv PANICS in a test whose goroutine
// tree went parallel — so freeing the family needs an env-read seam across 16
// production files, two of them load-bearing for secret redaction. That is
// #2808, with the sites and the witnesses it owes.
//
// The partition is per FILE, not per test: one serial test inside a parallel
// file is a 40-minute -race run that names nothing (the panic kills the
// binary), and one file / one answer is a rule a reader can check by reading
// one file.
//
// serialFamily below is a TABLE, not an exemption: it is the list of what
// remains to free. The guards keep the table and the code in step in both
// directions — a t.Setenv that appears in a file outside the table reddens,
// and a table line whose file no longer touches a global reddens as freed.
// Read at b06f9b290 with this campaign applied: 215 deterministic test files
// (a 216th is live-tagged), 75 serial by table and 2 by design, 138 parallel.
// That reading is prose on purpose — asserting the COUNT would redden every
// commit that adds a test file to this contended package, while the two
// directional guards already force each new file into a conscious answer.

// serialFamily names every test file that cannot run in parallel, with the
// WHOLE set of process-global calls that keep it there — the set, because a
// file held by both t.Setenv and a package global is a file the env seam alone
// does not free, which is the fact #2808 needs. Derived at b06f9b290 and
// checked against the code on every run:
//
//	66 files	Setenv
//	 3 files	SetCredentialsLookup, Setenv
//	 2 files	SetDefault (modelspecs, through seedSpecs)
//	 1 file 	SetCredentialsLookup, SetOAuthDirLookup, Setenv
//	 1 file 	SetCredentialsLookup, SetOAuthDirLookup
//	 1 file 	Setenv, Unsetenv
//	 1 file 	via memTest() in memory_tools_test.go (it sets ITERION_HOME)
//
// The day #2808 lands, lines leave this table and their tests gain
// t.Parallel() in the same commit. The two SetDefault files would need
// modelspecs.Default() threaded through the public ResolveCapabilities
// surface, which takes no ctx — a breaking signature change for two test
// files, refused here and recorded on #2789.
var serialFamily = map[string]string{
	"anthropic_forfait_ctx_test.go":            "SetCredentialsLookup, SetOAuthDirLookup, Setenv",
	"async_backend_test.go":                    "Setenv",
	"braced_env_reads_pin_test.go":             "Setenv",
	"capabilities_flags_test.go":               "Setenv",
	"capabilities_public_test.go":              "SetDefault",
	"claw_ambient_credential_header_test.go":   "Setenv",
	"claw_ambient_test.go":                     "Setenv",
	"claw_automem_note_test.go":                "Setenv",
	"claw_automem_secret_test.go":              "Setenv",
	"claw_backend_test.go":                     "Setenv",
	"claw_compress_secret_test.go":             "Setenv",
	"claw_drain_bound_override_test.go":        "Setenv",
	"claw_envfwd_empty_endpoint_test.go":       "Setenv",
	"claw_gateway_structured_output_test.go":   "Setenv",
	"claw_host_forfait_test.go":                "Setenv",
	"claw_kept_input_test.go":                  "Setenv",
	"claw_memory_secret_test.go":               "Setenv",
	"claw_node_session_linux_test.go":          "Setenv",
	"claw_openai_forfait_first_test.go":        "SetCredentialsLookup, Setenv",
	"claw_sandbox_env_class_test.go":           "Setenv",
	"claw_sandbox_env_test.go":                 "Setenv",
	"claw_vendored_provider_env_test.go":       "Setenv",
	"codex_client_version_test.go":             "Setenv",
	"cursors_test.go":                          "Setenv, Unsetenv",
	"executor_auto_memory_test.go":             "Setenv",
	"executor_cooldown_test.go":                "Setenv",
	"executor_effective_tools_test.go":         "Setenv",
	"executor_enginebin_test.go":               "Setenv",
	"executor_funding_test.go":                 "Setenv",
	"executor_noise_test.go":                   "Setenv",
	"executor_retry_classification_test.go":    "Setenv",
	"executor_retry_feedback_test.go":          "Setenv",
	"executor_test.go":                         "Setenv",
	"executor_timeout_test.go":                 "Setenv",
	"executor_tool_drain_bound_test.go":        "Setenv",
	"executor_tool_env_test.go":                "Setenv",
	"executor_tool_overlay_test.go":            "Setenv",
	"executor_tool_test.go":                    "Setenv",
	"executor_verified_action_test.go":         "Setenv",
	"expand_braced_env_linear_test.go":         "Setenv",
	"forfait_first_walk_test.go":               "Setenv",
	"gate_can_ask_test.go":                     "Setenv",
	"gateway_routing_test.go":                  "Setenv",
	"generation_test.go":                       "Setenv",
	"glm_routes_to_zai_test.go":                "SetCredentialsLookup, Setenv",
	"human_route_test.go":                      "Setenv",
	"memory_global_gate_test.go":               "via memTest() in memory_tools_test.go",
	"memory_tools_test.go":                     "Setenv",
	"model_override_test.go":                   "Setenv",
	"model_route_identity_test.go":             "Setenv",
	"modelspecs_merge_test.go":                 "SetDefault",
	"openai_forfait_ctx_test.go":               "SetCredentialsLookup, SetOAuthDirLookup",
	"openai_stream_usage_test.go":              "SetCredentialsLookup, Setenv",
	"override_fold_test.go":                    "Setenv",
	"permission_recovery_rung_test.go":         "Setenv",
	"process_env_policy_test.go":               "Setenv",
	"provider_allowlist_test.go":               "Setenv",
	"provider_hint_test.go":                    "Setenv",
	"recovery_route_test.go":                   "Setenv",
	"registry_identity_test.go":                "Setenv",
	"registry_global_lookup_test.go":           "SetCredentialsLookup, SetOAuthDirLookup",
	"registry_moonshot_test.go":                "Setenv",
	"registry_xai_test.go":                     "Setenv",
	"resolve_backend_test.go":                  "Setenv",
	"resolve_chain_pool_pins_test.go":          "Setenv",
	"resolve_chain_test.go":                    "Setenv",
	"route_view_dispatch_test.go":              "Setenv",
	"route_view_test.go":                       "Setenv",
	"routing_template_test.go":                 "Setenv",
	"secretguard_ambient_test.go":              "Setenv",
	"secretguard_minted_test.go":               "Setenv",
	"session_ledger_test.go":                   "Setenv",
	"settings_hooks_test.go":                   "Setenv",
	"slash_commands_test.go":                   "Setenv",
	"stream_error_wire_test.go":                "Setenv",
	"testparallel_test.go":                     "Setenv",
	"tool_attachment_deliverable_link_test.go": "Setenv",
	"tool_error_redaction_test.go":             "Setenv",
	"usage_presence_test.go":                   "Setenv",
	"wire_reach_test.go":                       "Setenv",
	"workflow_auto_test.go":                    "Setenv",
}

// serialByDesign names the shared process state the derived rule cannot see,
// one line per file. An entry whose file is already serial by derivation, or
// no longer exists, is stale and the guard says so — an exemption reaches the
// file its own line names and no other.
var serialByDesign = map[string]string{
	"claw_sandbox_crossing_test.go": "counts THIS process's open descriptors (/proc/self/fd) before and after a sandboxed run; any parallel test opening a file moves the count",
	"generation_tracing_test.go":    "errtrack installs its client once per PROCESS: these tests share one span transport and read the transactions appended since their own start",
}

// globalMutators are the calls that make a test's effect outlive its own
// goroutine. A new process-global setter in this package belongs here the day
// it lands, or the table it feeds stops meaning anything.
var globalMutators = map[string]bool{
	"Setenv":               true,
	"Unsetenv":             true,
	"Chdir":                true,
	"SetDefault":           true,
	"SetCredentialsLookup": true,
	"SetOAuthDirLookup":    true,
}

// readCampaign reads this package's own test files through the shared reader
// (internal/testparallel): the graph walk is the same one the bots modernize
// campaign needs, and a second copy of it is where a recopied guard drifts.
func readCampaign(t *testing.T) *testparallel.Campaign {
	t.Helper()
	c, err := testparallel.ReadCampaign(".", testparallel.LiveOnly)
	if err != nil {
		t.Fatalf("read this package's test files: %v", err)
	}
	if got := len(c.Files()); got < 100 {
		t.Fatalf("the walk read %d test files — it is judging nothing, so no verdict below means anything", got)
	}
	return c
}

// TestTheSerialFamilyTableSaysWhatTheCodeDoes keeps the table and the code in
// step in BOTH directions. The table is the backlog of #2808, so a file that
// quietly gains a t.Setenv has to join it, and a file that no longer touches a
// global has to leave it — with the t.Parallel() and the shard weight that
// freeing it justifies.
func TestTheSerialFamilyTableSaysWhatTheCodeDoes(t *testing.T) {
	t.Parallel()
	c := readCampaign(t)
	derived := c.FilesReaching(globalMutators)

	for f, why := range derived {
		if _, listed := serialFamily[f]; listed {
			continue
		}
		if _, byDesign := serialByDesign[f]; byDesign {
			t.Errorf("%s is serial by derivation (%s) AND listed in serialByDesign — the exemption contradicts the code, move the line to serialFamily", f, why)
			continue
		}
		t.Errorf("%s touches process-global state (%s) but is not in serialFamily — a test file cannot be both parallel and a global mutator: t.Setenv PANICS the -race binary. Add it to the table (and drop its t.Parallel calls), or stop touching the global", f, why)
	}
	for f, want := range serialFamily {
		got, ok := derived[f]
		switch {
		case !ok:
			t.Errorf("serialFamily names %s, which no longer touches any process global — it is FREED: drop the line, give its tests t.Parallel(), and move the partition reading with it", f)
		case got != want:
			t.Errorf("serialFamily[%s] = %q, the code says %q — the table records what blocks the file, so a changed blocker is a changed entry", f, want, got)
		}
	}
}

// TestASerialTestFileNeverGoesParallel is the half that keeps a 40-minute
// -race run from dying on a panic nobody can attribute.
func TestASerialTestFileNeverGoesParallel(t *testing.T) {
	t.Parallel()
	c := readCampaign(t)

	for _, f := range c.Files() {
		why, serial := serialFamily[f]
		if !serial {
			why, serial = serialByDesign[f]
		}
		if !serial {
			continue
		}
		for _, fn := range c.Tests(f) {
			if testparallel.OpensWithParallel(fn) {
				t.Errorf("%s:%s calls t.Parallel() in a serial file (%s) — a t.Setenv anywhere in its goroutine tree panics the whole test binary, and the panic names the test only if the binary gets that far",
					f, fn.Name.Name, why)
			}
		}
	}
}

// TestEveryParallelisableTestFileIsParallel is the campaign's ratchet: the
// width this package won is a rule, not a habit somebody keeps.
func TestEveryParallelisableTestFileIsParallel(t *testing.T) {
	t.Parallel()
	c := readCampaign(t)

	var missing []string
	for _, f := range c.Files() {
		if _, serial := serialFamily[f]; serial {
			continue
		}
		if _, byDesign := serialByDesign[f]; byDesign {
			continue
		}
		for _, fn := range c.Tests(f) {
			if !testparallel.OpensWithParallel(fn) {
				missing = append(missing, f+":"+fn.Name.Name)
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d test(s) in a parallelisable file do not open with t.Parallel() — the -race wall this package pays is the width they give back (#2789); add t.Parallel() as the first statement, or name what they share in serialFamily/serialByDesign:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestTheSerialByDesignExemptionsAreLive keeps the escape hatch honest: an
// exemption for a file the derived rule already covers, or for a file that no
// longer exists, reads as a reason that holds and covers nothing.
func TestTheSerialByDesignExemptionsAreLive(t *testing.T) {
	t.Parallel()
	c := readCampaign(t)
	present := map[string]bool{}
	for _, f := range c.Files() {
		present[f] = true
	}
	for f, why := range serialByDesign {
		switch {
		case !present[f]:
			t.Errorf("serialByDesign names %s, which this package no longer has — drop the line", f)
		case strings.TrimSpace(why) == "":
			t.Errorf("serialByDesign[%s] carries no reason", f)
		case len(c.Tests(f)) == 0:
			t.Errorf("serialByDesign names %s, which declares no top-level test — the exemption covers nothing", f)
		}
	}
}
