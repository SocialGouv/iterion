package bots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE MEASUREMENT READS WHAT THIS PASS PRODUCED. The scratch directory
// survives a resume, and a stack the survey stops naming between two passes
// leaves its extractor output behind: summing every file found there would add
// a previous pass's routes to this one's. Only the artefacts the runner names
// are read.
//
// Both directions on one bench: the same file, left in scratch, counts when
// the runner names it and does not count when it does not.
func TestAssessmentMeasureReadsOnlyTheOutputsThisPassProduced(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)
	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	stale := filepath.Join(scratch, "left-by-a-previous-pass.json")
	if err := os.WriteFile(stale, []byte(`{"stack": "gone", "extractor": "routes", `+
		`"facts": {"entrypoints": 500, "deployables": 9}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// No entrypoint and no deployable declared: the extracted counts are the
	// ones published, so a stale file would show.
	declarations := []map[string]any{
		{"id": "src-tree", "kind": "first_party", "path": "src"},
		{"id": "relational-store", "kind": "system", "identity": "relational-store", "path": "config/store.ini"},
	}
	survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef",
		[]map[string]any{{"id": "synth", "evidence": "a", "supported": true}}, declarations)

	run := func(t *testing.T, produced string) map[string]any {
		t.Helper()
		out, exit, stderr := assessmentRun(t, "measure", map[string]string{
			"{{vars.workspace_dir}}":     ws,
			"{{vars.scratch_dir}}":       scratch,
			"{{vars.profile_path}}":      "",
			"{{vars.bundle_skills_dir}}": bundleSkills(ws),
			"{{input.base_sha}}":         "deadbeefdeadbeef",
			"{{input.survey_path}}":      survey,
			"{{input.floor_path}}":       floor,
		}, map[string]string{
			"{{input.extractor_outputs}}":  produced,
			"{{input.stacks_unsupported}}": "[]",
			"{{input.stacks_covered}}":     "[]",
			"{{input.coverage_degraded}}":  "false",
			"{{input.coverage_missing}}":   "[]",
			"{{input.stacks_errored}}":     "[]",
		})
		if exit != 0 {
			t.Fatalf("measure exited %d: %s", exit, stderr)
		}
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("measure refused: %s", assessmentString(t, out, "reason"))
		}
		return out
	}

	t.Run("an output this pass did not produce is not read", func(t *testing.T) {
		facts := assessmentString(t, run(t, "[]"), "facts")
		if strings.Contains(facts, "500 entrypoints") || strings.Contains(facts, "9 deployed") {
			t.Fatalf("the measurement summed an extractor output left in scratch by another pass:\n%s", facts)
		}
	})
	t.Run("the same output, named by the runner, is read", func(t *testing.T) {
		named, err := json.Marshal([]map[string]string{{
			"stack": "gone", "extractor": "routes", "output": "left-by-a-previous-pass.json", "path": stale}})
		if err != nil {
			t.Fatal(err)
		}
		facts := assessmentString(t, run(t, string(named)), "facts")
		if !strings.Contains(facts, "500 entrypoints") {
			t.Fatalf("an output the runner named was not read — the control proves nothing:\n%s", facts)
		}
	})
}

// EVERY COUNT IS DISCRETE. The published uncertainty recomputes the index with
// one unit either way on each discrete metric; a small integer count left out
// of that list is a count whose one-unit disagreement never reaches the page,
// and `entrypoints` moves the index as much as the two beside it.
func TestAssessmentShippedProfileDeclaresEveryCountDiscrete(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("assessment", "skills", "measurement-profile.md"))
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)<!--\s*iterion:profile(.*?)-->`).FindSubmatch(body)
	if block == nil {
		t.Fatal("the shipped profile carries no iterion:profile block")
	}
	var profile struct {
		Metrics []struct {
			Key      string `json:"key"`
			Discrete bool   `json:"discrete"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(block[1], &profile); err != nil {
		t.Fatalf("the profile block is not JSON: %v", err)
	}
	seen := map[string]bool{}
	for _, metric := range profile.Metrics {
		seen[metric.Key] = true
		wantDiscrete := metric.Key != "first_party_lines"
		if metric.Discrete != wantDiscrete {
			t.Errorf("metric %q declares discrete=%v, want %v: a count of things moves by whole "+
				"units, a count of lines does not", metric.Key, metric.Discrete, wantDiscrete)
		}
	}
	for _, key := range []string{"first_party_lines", "entrypoints", "deployables", "systems"} {
		if !seen[key] {
			t.Errorf("the shipped profile no longer declares %q", key)
		}
	}
}
