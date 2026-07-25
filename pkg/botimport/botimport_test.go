package botimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures are ORIGINAL test material authored for this repo — never
// copied from third-party workflow repos.
var fixtureNames = []string{"simple_fixloop", "pipeline_parallel", "unknown_constructs"}

func importFixture(t *testing.T, name string) *Result {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", name+".js"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res, err := Import(name+".js", src, Options{})
	if err != nil {
		t.Fatalf("Import(%s): %v", name, err)
	}
	return res
}

func TestImportFixturesAgainstGoldens(t *testing.T) {
	update := os.Getenv("UPDATE_GOLDENS") == "1"
	for _, name := range fixtureNames {
		t.Run(name, func(t *testing.T) {
			res := importFixture(t, name)
			golden := filepath.Join("testdata", "goldens", name+".bot")
			if update {
				if err := os.WriteFile(golden, []byte(res.BotSource), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with UPDATE_GOLDENS=1 to record): %v", err)
			}
			if res.BotSource != string(want) {
				t.Fatalf("draft drifted from golden %s.\n--- got ---\n%s", golden, res.BotSource)
			}
		})
	}
}

// TestImportSucceedsWithoutDetectableCredentials pins the fix for the
// credential-environment-dependent CI failure. A generated draft's agents
// carry no `backend:` (a lossy import leaves that choice for the operator), so
// ir.Compile emits C018 as an ERROR whenever the host has no auto-detectable
// credential. validateDraft must NOT treat that host-environment property as
// "the draft does not compile" — otherwise TestImportFixturesAgainstGoldens
// above passes on a credentialed dev box and fails on a credential-less CI
// runner. This test reproduces the CI environment by scrubbing every
// credential detect.Detect reads (same scrub set as
// ir.TestCompileSupervisorModelFallbackMissing), and asserts every fixture
// still imports.
func TestImportSucceedsWithoutDetectableCredentials(t *testing.T) {
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL", "ZAI_API_KEY",
		"OPENAI_API_KEY",
		"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
		"AWS_REGION", "AWS_DEFAULT_REGION",
		"GOOGLE_CLOUD_PROJECT",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"ITERION_BACKEND_PREFERENCE",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())

	for _, name := range fixtureNames {
		src, err := os.ReadFile(filepath.Join("testdata", name+".js"))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		if _, err := Import(name+".js", src, Options{}); err != nil {
			t.Fatalf("Import(%s) must succeed without detectable credentials "+
				"(C018 is a host-env property, not a draft compile error): %v", name, err)
		}
	}
}

func TestRoundtripStability(t *testing.T) {
	// Importing the same source twice must produce byte-identical
	// drafts (goldens depend on it, and so does operator trust).
	for _, name := range fixtureNames {
		src, err := os.ReadFile(filepath.Join("testdata", name+".js"))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		a, err := Import(name+".js", src, Options{})
		if err != nil {
			t.Fatalf("first import: %v", err)
		}
		b, err := Import(name+".js", src, Options{})
		if err != nil {
			t.Fatalf("second import: %v", err)
		}
		if a.BotSource != b.BotSource {
			t.Fatalf("%s: import is not deterministic", name)
		}
	}
}

func TestSimpleFixloopShape(t *testing.T) {
	res := importFixture(t, "simple_fixloop")
	src := res.BotSource

	for _, want := range []string{
		"workflow lint_fix_loop:",
		"entry: surveyor",
		"schema survey_schema:",
		`severity: string [enum: "none", "minor", "major"]`,
		"hotspots: string[]",
		"remaining: int",
		`surveyor -> done when "severity == 'none'"`,
		"surveyor -> fixer else",
		`checker -> fixer when "verdict != 'clean'" as loop_1(4)`,
		"checker -> wrapper else",
		"wrapper -> done",
		"{{vars.target}}",
		"{{outputs.surveyor.summary}}",
		"{{vars.hole_1}}",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("draft missing %q\n--- draft ---\n%s", want, src)
		}
	}
	if res.WorkflowName != "lint_fix_loop" {
		t.Errorf("workflow name = %q", res.WorkflowName)
	}
	if !res.Report.NeedsAttention() {
		t.Error("a lossy import with holes must flag NeedsAttention")
	}
}

func TestPipelineParallelShape(t *testing.T) {
	res := importFixture(t, "pipeline_parallel")
	src := res.BotSource
	for _, want := range []string{
		"router fan_1:",
		"mode: fan_out_each",
		`over: "{{outputs.planner.modules}}"`,
		"as: m",
		"{{outputs.fan_1.m}}",
		"router fan_2:",
		"mode: fan_out_all",
		"tool join_1:",
		"await: wait_all",
		"planner -> fan_1",
		"fan_1 -> rewriter",
		"rewriter -> join_1",
		"join_1 -> fan_2",
		"fan_2 -> linkcheck",
		"fan_2 -> spellcheck",
		"linkcheck -> join_2",
		"spellcheck -> join_2",
		"join_2 -> done",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("draft missing %q\n--- draft ---\n%s", want, src)
		}
	}
}

func TestUnknownConstructAsPlaceholder(t *testing.T) {
	// The contract: never crash, never invent — degrade visibly.
	res := importFixture(t, "unknown_constructs")
	if len(res.Report.Placeholders) == 0 {
		t.Fatal("expected placeholders for unmapped constructs")
	}
	if len(res.Report.Dropped) == 0 {
		t.Fatal("expected dropped entries (helper fn, try/finally)")
	}
	if !strings.Contains(res.BotSource, "## IMPORT") {
		t.Fatal("draft must carry IMPORT markers")
	}
	// The two real agents still made it through.
	for _, want := range []string{"agent risky:", "agent finisher:", "risky -> finisher", "finisher -> done"} {
		if !strings.Contains(res.BotSource, want) {
			t.Errorf("draft missing %q", want)
		}
	}
}

func TestImportErrors(t *testing.T) {
	if _, err := Import("bad.js", []byte("const x = {{{"), Options{}); err == nil {
		t.Error("unparsable JS must error")
	}
	if _, err := Import("empty.js", []byte("const x = 1\nlog('hi')"), Options{}); err == nil {
		t.Error("script without agent calls must error, not emit an empty workflow")
	}
}

func TestNameOverride(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "pipeline_parallel.js"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Import("pipeline_parallel.js", src, Options{Name: "My Sweep"})
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkflowName != "my_sweep" {
		t.Errorf("name override → %q, want my_sweep", res.WorkflowName)
	}
	if !strings.Contains(res.BotSource, "workflow my_sweep:") {
		t.Error("override not reflected in the draft")
	}
}
