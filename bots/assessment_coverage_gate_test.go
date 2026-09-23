package bots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The per-stack layer carries no language in the workflow: the runner reads
// the `iterion:extractors` block of `stack-<id>.md`, and the coverage gate
// reads the SAME blocks independently. These tests pin the two properties that
// make the arrangement worth anything:
//
//   - UNSUPPORTED IS NOT ZERO. A stack nobody shipped a skill for is reported
//     by name. A zero would read as a fact about the repository; it is a fact
//     about the bundle.
//   - A stack we DO ship a skill for, that produced none of its declared
//     outputs, is a coverage void and hard-refuses. Measured on the security
//     bot this pattern comes from: a dispatcher silently ran zero scanners and
//     the run still read "healthy".

// aStackSkill is a synthetic stack skill in the shipped format: one
// machine-readable spec block, one fenced script per extractor.
const aStackSkill = "---\nname: stack-synth\ndescription: synthetic fixture\n---\n\n" +
	"# stack-synth\n\n" +
	"<!-- iterion:extractors\n" +
	`[{"id":"counts","output":"synth-counts.json","emits":["entrypoints"],"interpreter":"python3"}]` +
	"\n-->\n\n" +
	"<!-- iterion:script counts -->\n\n" +
	"```python\n" +
	"import json\n" +
	`print(json.dumps({"stack": "synth", "extractor": "counts", "facts": {"entrypoints": 7, "deployables": 2}}))` +
	"\n```\n"

// aSilentStackSkill declares an extractor whose script writes nothing at all —
// the façade the gate exists to catch.
const aSilentStackSkill = "---\nname: stack-silent\ndescription: synthetic fixture\n---\n\n" +
	"# stack-silent\n\n" +
	"<!-- iterion:extractors\n" +
	`[{"id":"counts","output":"silent-counts.json","emits":["entrypoints"],"interpreter":"python3"}]` +
	"\n-->\n\n" +
	"<!-- iterion:script counts -->\n\n" +
	"```python\n" +
	"import sys\n" +
	`sys.stderr.write("nothing to report")` +
	"\n```\n"

// stackWorkspace lays out a workspace the way the runtime does: the bundle's
// skills mirrored under .claude/skills.
func stackWorkspace(t *testing.T, skills map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range skills {
		if err := os.WriteFile(filepath.Join(skillsDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runExtractors(t *testing.T, ws, scratch string, stacks []map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(stacks)
	if err != nil {
		t.Fatal(err)
	}
	out, exit, stderr := assessmentRun(t, "run_extractors",
		map[string]string{"{{vars.workspace_dir}}": ws, "{{vars.scratch_dir}}": scratch},
		map[string]string{"{{input.stacks}}": string(raw)})
	if exit != 0 {
		t.Fatalf("run_extractors exited %d: %s", exit, stderr)
	}
	return out
}

func runHealth(t *testing.T, ws, scratch, surveyPath string, extract map[string]any) map[string]any {
	t.Helper()
	outputs, _ := json.Marshal(extract["outputs"])
	unsupported, _ := json.Marshal(extract["unsupported"])
	out, exit, stderr := assessmentRun(t, "inventory_health",
		map[string]string{"{{vars.workspace_dir}}": ws, "{{vars.scratch_dir}}": scratch,
			"{{input.survey_path}}": surveyPath},
		map[string]string{"{{input.outputs}}": string(outputs),
			"{{input.unsupported}}": string(unsupported)})
	if exit != 0 {
		t.Fatalf("inventory_health exited %d: %s", exit, stderr)
	}
	return out
}

func TestAssessmentExtractorsRunFromTheSkillAlone(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill})
	scratch := t.TempDir()
	stacks := []map[string]any{{"id": "synth", "evidence": "README.md", "supported": true}}

	out := runExtractors(t, ws, scratch, stacks)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the runner refused a well-formed stack skill: %s", assessmentString(t, out, "reason"))
	}
	produced := filepath.Join(scratch, "synth-counts.json")
	body, err := os.ReadFile(produced)
	if err != nil {
		t.Fatalf("the declared output was not written: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("the extractor's output is not JSON: %v", err)
	}
	// Adding a stack is dropping a file: nothing in main.bot named this one.
	if document["stack"] != "synth" {
		t.Fatalf("output came from %v, want the fixture stack", document["stack"])
	}
}

func TestAssessmentUnsupportedStackIsNamedNotZeroed(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill})
	scratch := t.TempDir()
	stacks := []map[string]any{
		{"id": "synth", "evidence": "a", "supported": true},
		{"id": "unknownstack", "evidence": "b", "supported": false, "reason": "no skill here"},
	}

	out := runExtractors(t, ws, scratch, stacks)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("a stack with no skill made the runner fail outright: %s", assessmentString(t, out, "reason"))
	}
	unsupported, _ := out["unsupported"].([]any)
	if len(unsupported) != 1 {
		t.Fatalf("unsupported = %v, want exactly the stack with no skill", out["unsupported"])
	}
	entry, _ := unsupported[0].(map[string]any)
	if entry["stack"] != "unknownstack" {
		t.Fatalf("the unsupported stack is not named: %v", entry)
	}

	survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)
	health := runHealth(t, ws, scratch, survey, out)
	if !assessmentBool(t, health, "ok") {
		t.Fatalf("an unsupported stack hard-failed the gate: %s", assessmentString(t, health, "reason"))
	}
	if !assessmentBool(t, health, "degraded") {
		t.Fatal("a run carrying an unsupported stack reported UNDEGRADED — the gap would be invisible downstream")
	}
	if assessmentBool(t, health, "healthy") {
		t.Fatal("a run carrying an unsupported stack reported healthy")
	}
	if !strings.Contains(assessmentString(t, health, "reason"), "unsupported") {
		t.Errorf("the verdict does not mention the unsupported stack: %s", assessmentString(t, health, "reason"))
	}
}

// The façade: a skill this bundle SHIPS, so the bot claims to measure that
// stack, whose extractor ran and wrote nothing. A zero here would be read as a
// property of the repository.
func TestAssessmentCoverageVoidRefuses(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-silent.md": aSilentStackSkill})
	scratch := t.TempDir()
	stacks := []map[string]any{{"id": "silent", "evidence": "a", "supported": true}}

	out := runExtractors(t, ws, scratch, stacks)
	if len(out["errors"].([]any)) == 0 {
		t.Fatal("an extractor that wrote no JSON was recorded without an error — the artefact is what must be verified, not the exit code")
	}

	survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)
	health := runHealth(t, ws, scratch, survey, out)
	if assessmentBool(t, health, "ok") {
		t.Fatal("a stack this bundle ships a skill for produced NONE of its declared outputs and the gate reported healthy")
	}
	reason := assessmentString(t, health, "reason")
	if !strings.Contains(reason, "silent") {
		t.Errorf("the refusal does not name the void stack: %s", reason)
	}
	if assessmentString(t, health, "code") != "COVERAGE_VOID" {
		t.Errorf("code = %q, want COVERAGE_VOID", assessmentString(t, health, "code"))
	}
}

// The gate reads the SKILLS, never the runner's report. A gate that believed
// the runner would agree with a runner that parsed nothing and ran nothing —
// which is the one failure it exists to catch. This test forges the report.
func TestAssessmentCoverageGateDoesNotBelieveTheRunner(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill})
	scratch := t.TempDir()
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}
	survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)

	// A runner that ran nothing and says so by saying nothing: empty outputs,
	// empty unsupported. If the gate took its expectations from this, it would
	// find nothing missing and report healthy.
	forged := map[string]any{"outputs": map[string]any{}, "unsupported": []any{}}
	health := runHealth(t, ws, scratch, survey, forged)
	if assessmentBool(t, health, "ok") {
		t.Fatal("the gate accepted a runner report claiming nothing ran and nothing was expected — it is deriving its expectations from the report, not from the skills")
	}
	if assessmentString(t, health, "code") != "COVERAGE_VOID" {
		t.Errorf("code = %q, want COVERAGE_VOID", assessmentString(t, health, "code"))
	}
}

// The two readers of the same block are written twice, deliberately. Written
// twice, they can DRIFT — so the drift is what gets pinned: over the bundle's
// own shipped stack skills, the runner produces exactly the outputs the gate
// expects, and neither can be edited alone without reddening here.
func TestAssessmentShippedStackSkillsParseTheSameBothWays(t *testing.T) {
	requireAssessmentTools(t)
	shipped, err := filepath.Glob("assessment/skills/stack-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipped) == 0 {
		t.Fatal("no stack-*.md shipped in the assessment bundle — the per-stack layer would be a claim")
	}

	skills := map[string]string{}
	stacks := []map[string]any{}
	for _, path := range shipped {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(path)
		skills[name] = string(body)
		stacks = append(stacks, map[string]any{
			"id":        strings.TrimSuffix(strings.TrimPrefix(name, "stack-"), ".md"),
			"evidence":  "README.md",
			"supported": true,
		})
	}

	// An empty git repository: every shipped extractor must still produce a
	// readable artefact on a tree with nothing in it. An extractor that only
	// works on a repository of the right shape is a coverage gap with a
	// schedule.
	ws := stackWorkspace(t, skills)
	gitInitBare(t, ws)
	scratch := t.TempDir()

	out := runExtractors(t, ws, scratch, stacks)
	if errs, _ := out["errors"].([]any); len(errs) > 0 {
		t.Fatalf("shipped extractors errored on an empty repository: %v", errs)
	}
	survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)
	health := runHealth(t, ws, scratch, survey, out)
	if !assessmentBool(t, health, "ok") {
		t.Fatalf("the gate's expectations disagree with what the runner produced from the same blocks: %s",
			assessmentString(t, health, "reason"))
	}
	if missing, _ := health["missing"].([]any); len(missing) > 0 {
		t.Fatalf("the gate expects outputs the runner did not produce: %v", missing)
	}
}

// gitInitBare turns a directory into an empty git repository, so an extractor
// that shells `git ls-files` has something to answer against.
func gitInitBare(t *testing.T, dir string) {
	t.Helper()
	gittest.Run(t, dir, "init", "-q", "-b", "main")
	gittest.Run(t, dir, "config", "user.email", "t@example.com")
	gittest.Run(t, dir, "config", "user.name", "t")
	gittest.Run(t, dir, "config", "commit.gpgsign", "false")
}
