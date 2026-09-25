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
// skills in the ENGINE-OWNED copy the engine resets and refills on every
// mirror pass, and the workspace-wins mirror beside it carrying the audited
// repository's own version of the same names.
//
// The second half is not decoration. It is what makes the tests below able to
// tell which directory a node read: a fixture with only one copy passes
// whichever one it reads.
func stackWorkspace(t *testing.T, skills map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeSkills(t, bundleSkills(dir), skills)
	hostile := map[string]string{}
	for name := range skills {
		hostile[name] = aRepositorySuppliedSkill
	}
	writeSkills(t, filepath.Join(dir, ".claude", "skills"), hostile)
	return dir
}

// bundleSkills is the engine-owned copy: `${BUNDLE_SKILLS_DIR}`.
func bundleSkills(workspace string) string {
	return filepath.Join(workspace, ".claude", "iterion-skills")
}

func writeSkills(t *testing.T, dir string, skills map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range skills {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// aRepositorySuppliedSkill is what an audited checkout can put under the name
// of a skill this bundle ships: a different extractor set, a different script.
// Nothing in it is hostile — it only has to be DIFFERENT, so that a node
// reading it instead of the bundle's copy is visible.
const aRepositorySuppliedSkill = "---\nname: stack-synth\ndescription: supplied by the checkout\n---\n\n" +
	"<!-- iterion:extractors\n" +
	`[{"id":"counts","output":"supplied-by-the-checkout.json","emits":["entrypoints"],"interpreter":"python3"}]` +
	"\n-->\n\n" +
	"<!-- iterion:script counts -->\n\n" +
	"```python\n" +
	"import json\n" +
	`print(json.dumps({"stack": "synth", "extractor": "counts", "facts": {"entrypoints": 9999}}))` +
	"\n```\n"

func runExtractors(t *testing.T, ws, scratch string, stacks []map[string]any) map[string]any {
	t.Helper()
	return runExtractorsAt(t, ws, scratch, stacks, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
}

// runExtractorsAt pins the pass to a commit, the way the workflow does: the
// skills declare that their scripts read the tree AT $BASE_SHA, and an
// extractor handed an empty one would measure the checkout.
func runExtractorsAt(t *testing.T, ws, scratch string, stacks []map[string]any, sha string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(stacks)
	if err != nil {
		t.Fatal(err)
	}
	out, exit, stderr := assessmentRun(t, "run_extractors",
		map[string]string{"{{vars.workspace_dir}}": ws, "{{vars.scratch_dir}}": scratch,
			"{{vars.bundle_skills_dir}}": bundleSkills(ws), "{{input.base_sha}}": sha},
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
	errs := extract["errors"]
	if errs == nil {
		errs = []any{}
	}
	errors, _ := json.Marshal(errs)
	out, exit, stderr := assessmentRun(t, "inventory_health",
		map[string]string{"{{vars.workspace_dir}}": ws, "{{vars.scratch_dir}}": scratch,
			"{{vars.bundle_skills_dir}}": bundleSkills(ws), "{{input.survey_path}}": surveyPath},
		map[string]string{"{{input.outputs}}": string(outputs),
			"{{input.unsupported}}": string(unsupported),
			"{{input.errors}}":      string(errors)})
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
	sha := gitInitEmptyCommit(t, ws)
	scratch := t.TempDir()

	out := runExtractorsAt(t, ws, scratch, stacks, sha)
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

// gitInitEmptyCommit turns a directory into a git repository carrying ONE
// empty commit, and returns its sha. The extractors read the tree at a pinned
// commit, so a repository with no commit has nothing for them to read — and a
// bundle-shipped extractor must still produce a readable artefact over an
// empty tree.
func gitInitEmptyCommit(t *testing.T, dir string) string {
	t.Helper()
	gittest.Run(t, dir, "init", "-q", "-b", "main")
	gittest.Run(t, dir, "config", "user.email", "t@example.com")
	gittest.Run(t, dir, "config", "user.name", "t")
	gittest.Run(t, dir, "config", "commit.gpgsign", "false")
	gittest.Run(t, dir, "commit", "-q", "--allow-empty", "-m", "empty")
	return strings.TrimSpace(gittest.Run(t, dir, "rev-parse", "HEAD"))
}

// TWO READINGS OF ONE QUESTION, and the point of having two is that they can
// disagree. The published gap used to come from the agent's flag alone: on a
// repository whose whole stack this bundle has no extractor for, the document
// read "stacks NOT covered: none" while the deterministic layer knew better.
func TestAssessmentCoverageDivergenceIsRefusedNotAveraged(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill})

	t.Run("the survey claims coverage this bundle does not have", func(t *testing.T) {
		scratch := t.TempDir()
		stacks := []map[string]any{
			{"id": "synth", "evidence": "a", "supported": true},
			// No stack-elsewhere.md ships here, and the survey says otherwise.
			{"id": "elsewhere", "evidence": "b", "supported": true},
		}
		out := runExtractors(t, ws, scratch, stacks)
		survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)
		health := runHealth(t, ws, scratch, survey, out)
		if assessmentBool(t, health, "ok") {
			t.Fatal("the survey declared a stack SUPPORTED that this bundle has no extractor for, and the gate agreed with both")
		}
		if !assessmentBool(t, health, "divergent") {
			t.Fatal("the disagreement was not routed as one — it would reach the same fail node as a plain coverage void")
		}
		if code := assessmentString(t, health, "code"); code != "COVERAGE_DIVERGENCE" {
			t.Fatalf("code = %q, want COVERAGE_DIVERGENCE", code)
		}
		if !strings.Contains(assessmentString(t, health, "reason"), "elsewhere") {
			t.Errorf("the refusal does not name the stack: %s", assessmentString(t, health, "reason"))
		}
	})

	t.Run("the survey writes off a stack this bundle measured", func(t *testing.T) {
		scratch := t.TempDir()
		stacks := []map[string]any{
			{"id": "synth", "evidence": "a", "supported": false, "reason": "believed uncovered"},
		}
		out := runExtractors(t, ws, scratch, stacks)
		survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)
		health := runHealth(t, ws, scratch, survey, out)
		if assessmentBool(t, health, "ok") {
			t.Fatal("a stack declared unsupported, whose extractors this bundle ran, was accepted — the document would publish a gap that is not there")
		}
		if code := assessmentString(t, health, "code"); code != "COVERAGE_DIVERGENCE" {
			t.Fatalf("code = %q, want COVERAGE_DIVERGENCE", code)
		}
	})

	// The control: the two agree, and the union is what gets published.
	t.Run("the two agree and the union is published", func(t *testing.T) {
		scratch := t.TempDir()
		stacks := []map[string]any{
			{"id": "synth", "evidence": "a", "supported": true},
			{"id": "elsewhere", "evidence": "b", "supported": false, "reason": "no extractor skill in this bundle"},
		}
		out := runExtractors(t, ws, scratch, stacks)
		survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)
		health := runHealth(t, ws, scratch, survey, out)
		if !assessmentBool(t, health, "ok") {
			t.Fatalf("the gate refused two readings that agree: %s", assessmentString(t, health, "reason"))
		}
		if assessmentBool(t, health, "divergent") {
			t.Fatal("two readings that agree were reported as divergent")
		}
		union, _ := health["stacks_unsupported"].([]any)
		if len(union) != 1 {
			t.Fatalf("stacks_unsupported = %v, want the one stack neither side covers", health["stacks_unsupported"])
		}
	})
}

// An interpreter a shipped skill declares and this bundle does not provision
// is a PACKAGING defect. Left unnamed it produces no output, which reads
// downstream as a coverage void — a fact about the repository rather than
// about the bundle, and the operator is sent to the wrong file.
func TestAssessmentUnprovisionedInterpreterIsNamed(t *testing.T) {
	requireAssessmentTools(t)
	skill := strings.Replace(aStackSkill, `"interpreter":"python3"`,
		`"interpreter":"an-interpreter-nobody-installed"`, 1)
	if skill == aStackSkill {
		t.Fatal("the mutation did not apply")
	}
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": skill})
	out := runExtractors(t, ws, t.TempDir(), []map[string]any{
		{"id": "synth", "evidence": "a", "supported": true}})
	if assessmentBool(t, out, "ok") {
		t.Fatal("a skill declaring an interpreter this bundle does not ship ran to a silent zero")
	}
	reason := assessmentString(t, out, "reason")
	if !strings.Contains(reason, "an-interpreter-nobody-installed") {
		t.Errorf("the refusal does not name the interpreter: %s", reason)
	}
	if !strings.Contains(reason, "devbox.json") {
		t.Errorf("the refusal does not say where to provision it: %s", reason)
	}
}

// The pin is not decoration: an extractor handed no commit measures the
// CHECKOUT, and the document says "measured at commit" over it.
func TestAssessmentExtractorsRefuseAnUnpinnedPass(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill})
	out := runExtractorsAt(t, ws, t.TempDir(),
		[]map[string]any{{"id": "synth", "evidence": "a", "supported": true}}, "")
	if assessmentBool(t, out, "ok") {
		t.Fatal("the extractor runner measured with no pinned commit — it would read the checkout")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "pinned commit") {
		t.Errorf("the refusal does not name the missing pin: %s", assessmentString(t, out, "reason"))
	}
}

// THE EXTRACTORS MEASURE THE COMMIT. Every skill in this bundle says its
// scripts read the tree at `$BASE_SHA`, and the rendered document says
// "measured at commit"; a script reading the checkout instead counts build
// output, an installed dependency tree and whatever a previous node left
// behind — none of which is in the commit it claims to describe.
//
// Proven in BOTH directions on a throwaway repository: the pinned pass does
// not see the working tree's edit, and it does see what the commit holds.
func TestAssessmentShippedExtractorsReadTheCommitNotTheCheckout(t *testing.T) {
	requireAssessmentTools(t)
	skills := map[string]string{}
	for _, name := range []string{"stack-go.md", "stack-node.md"} {
		body, err := os.ReadFile(filepath.Join("assessment", "skills", name))
		if err != nil {
			t.Fatal(err)
		}
		skills[name] = string(body)
	}
	ws := stackWorkspace(t, skills)
	gittest.Run(t, ws, "init", "-q", "-b", "main")
	gittest.Run(t, ws, "config", "user.email", "t@example.com")
	gittest.Run(t, ws, "config", "user.name", "t")
	gittest.Run(t, ws, "config", "commit.gpgsign", "false")

	write := func(rel, body string) {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/app\n\ngo 1.21\n")
	write("package.json", `{"name": "app", "engines": {"node": "20.0.0"}}`)
	gittest.Run(t, ws, "add", "go.mod", "package.json")
	gittest.Run(t, ws, "commit", "-qm", "the commit under assessment")
	sha := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))

	// What a checkout accumulates and a commit never holds: an edit in flight,
	// and an installed dependency tree carrying its own manifests.
	write("go.mod", "module example.test/app\n\ngo 9.99\n")
	write("node_modules/left-pad/package.json", `{"name": "left-pad", "engines": {"node": "0.1.0"}}`)
	write("vendored-build/go.mod", "module example.test/build\n\ngo 8.88\n")

	scratch := t.TempDir()
	out := runExtractorsAt(t, ws, scratch, []map[string]any{
		{"id": "go", "evidence": "go.mod", "supported": true},
		{"id": "node", "evidence": "package.json", "supported": true},
	}, sha)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the shipped extractors refused a pinned pass: %s", assessmentString(t, out, "reason"))
	}
	if errs, _ := out["errors"].([]any); len(errs) > 0 {
		t.Fatalf("the shipped extractors errored on a pinned pass: %v", errs)
	}

	for name, wantVersion := range map[string]string{
		"go-modules.json": "1.21", "node-packages.json": "20.0.0",
	} {
		body, err := os.ReadFile(filepath.Join(scratch, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := string(body)
		if !strings.Contains(text, wantVersion) {
			t.Errorf("%s does not carry the version the COMMIT declares (%s):\n%s", name, wantVersion, text)
		}
		for _, fromTheCheckout := range []string{"9.99", "8.88", "0.1.0", "left-pad"} {
			if strings.Contains(text, fromTheCheckout) {
				t.Errorf("%s carries %q, which exists only in the working directory — the "+
					"extractor is measuring the checkout, and the document says `measured at commit`",
					name, fromTheCheckout)
			}
		}
	}
}

// THE BLOCKS THIS BUNDLE EXECUTES COME FROM THE BUNDLE. The workspace is a
// checkout of the repository under assessment, and `<workspace>/.claude/skills`
// applies the workspace-wins collision policy: a skill read there may be the
// bundle's or the audited repository's, and nothing read back tells the two
// apart. This bot RUNS the script a skill anchors and takes the measurement
// SCALE from another, so both go through the engine-owned copy.
//
// The fixture carries both directories with the same file names — a fixture
// with one copy passes whichever one is read.
func TestAssessmentSkillBlocksComeFromTheBundleNotTheCheckout(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill})
	scratch := t.TempDir()
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}

	out := runExtractors(t, ws, scratch, stacks)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the runner refused: %s", assessmentString(t, out, "reason"))
	}
	if _, err := os.Stat(filepath.Join(scratch, "synth-counts.json")); err != nil {
		t.Fatalf("the BUNDLE's declared output was not produced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scratch, "supplied-by-the-checkout.json")); err == nil {
		t.Fatal("the runner produced the output the CHECKOUT's copy of the skill declares — it " +
			"executed a script the repository under assessment supplied, with this run's environment")
	}

	// The coverage gate derives its expectations from the same blocks, read
	// independently. Read from the checkout they would be the checkout's.
	survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)
	health := runHealth(t, ws, scratch, survey, out)
	if !assessmentBool(t, health, "ok") {
		t.Fatalf("the gate's expectations do not match what the runner produced from the bundle: %s",
			assessmentString(t, health, "reason"))
	}

	// And with no engine-owned copy at all, neither node falls back.
	t.Run("no bundle copy is a refusal, never a fall back to the workspace", func(t *testing.T) {
		outNoBundle, exit, stderr := assessmentRun(t, "run_extractors",
			map[string]string{"{{vars.workspace_dir}}": ws, "{{vars.scratch_dir}}": t.TempDir(),
				"{{vars.bundle_skills_dir}}": "", "{{input.base_sha}}": "deadbeef"},
			map[string]string{"{{input.stacks}}": `[{"id":"synth","evidence":"a","supported":true}]`})
		if exit != 0 {
			t.Fatalf("run_extractors exited %d: %s", exit, stderr)
		}
		if assessmentBool(t, outNoBundle, "ok") {
			t.Fatal("the runner ran with no engine-owned skills directory — it read the workspace")
		}
	})
}

// The SCALE is a bundle artefact too. A measurement profile the audited
// repository supplies would move the published letter under the bundle's own
// profile name, and a letter is only meaningful as a ratio to the anchor of
// the profile it names.
func TestAssessmentMeasurementProfileComesFromTheBundle(t *testing.T) {
	requireAssessmentTools(t)
	profile, err := os.ReadFile(filepath.Join("assessment", "skills", "measurement-profile.md"))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	writeSkills(t, bundleSkills(ws), map[string]string{"measurement-profile.md": string(profile)})
	// The audited checkout's own copy, under the same name, with an anchor a
	// hundredth of the bundle's: every repository would size two bands up.
	writeSkills(t, filepath.Join(ws, ".claude", "skills"), map[string]string{
		"measurement-profile.md": strings.Replace(string(profile),
			`"first_party_lines": 20000`, `"first_party_lines": 200`, 1)})

	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}
	survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, inDomainSurvey(2))
	out := measure(t, ws, scratch, survey, floor)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("measure refused: %s", assessmentString(t, out, "reason"))
	}
	if index, _ := out["index"].(float64); index > 1.0 {
		t.Fatalf("index = %v: the measurement took its anchor from the checkout's profile, and "+
			"published the letter under the bundle's profile name", index)
	}
}

// An operator MAY supply their own profile — that is a launch-time decision.
// It may not wear the bundle's identity: a letter computed against another
// anchor, published as `public-default`, is quoted and compared as one nobody
// computed.
func TestAssessmentAnOperatorProfileMayNotWearTheBundlesName(t *testing.T) {
	requireAssessmentTools(t)
	profile, err := os.ReadFile(filepath.Join("assessment", "skills", "measurement-profile.md"))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	writeSkills(t, bundleSkills(ws), map[string]string{"measurement-profile.md": string(profile)})
	if err := os.WriteFile(filepath.Join(ws, "mine.md"),
		[]byte(strings.Replace(string(profile), `"first_party_lines": 20000`, `"first_party_lines": 200`, 1)),
		0o644); err != nil {
		t.Fatal(err)
	}

	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}
	survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, inDomainSurvey(2))
	out, exit, stderr := assessmentRun(t, "measure", map[string]string{
		"{{vars.workspace_dir}}":     ws,
		"{{vars.scratch_dir}}":       scratch,
		"{{vars.profile_path}}":      "mine.md",
		"{{vars.bundle_skills_dir}}": bundleSkills(ws),
		"{{input.base_sha}}":         "deadbeefdeadbeef",
		"{{input.survey_path}}":      survey,
		"{{input.floor_path}}":       floor,
	}, map[string]string{
		"{{input.extractor_outputs}}":  "[]",
		"{{input.stacks_unsupported}}": "[]",
		"{{input.stacks_covered}}":     "[]",
		"{{input.coverage_degraded}}":  "false",
		"{{input.coverage_missing}}":   "[]",
		"{{input.stacks_errored}}":     "[]",
	})
	if exit != 0 {
		t.Fatalf("measure exited %d: %s", exit, stderr)
	}
	if assessmentBool(t, out, "ok") {
		t.Fatal("a supplied profile published its letter under the bundle's own profile identity")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "own id") {
		t.Errorf("the refusal does not say what to do: %s", assessmentString(t, out, "reason"))
	}
}

// WHAT A BUILD MANIFEST LOOKS LIKE IS STACK KNOWLEDGE. The floor enumerated a
// dozen ecosystems' manifest names in the workflow, which is the shape the
// catalog's universality rule forbids: teaching this bot about a new ecosystem
// would have been an edit to the DSL. The shapes are declared by the skills
// now, and the floor takes the union.
func TestAssessmentFloorTakesItsManifestShapesFromTheSkills(t *testing.T) {
	requireAssessmentTools(t)

	runFloor := func(t *testing.T, ws, scratch, sha string) map[string]any {
		t.Helper()
		out, exit, stderr := assessmentRun(t, "inventory_floor", map[string]string{
			"{{vars.workspace_dir}}":     ws,
			"{{vars.scratch_dir}}":       scratch,
			"{{vars.bundle_skills_dir}}": bundleSkills(ws),
			"{{input.base_sha}}":         sha,
		}, nil)
		if exit != 0 {
			t.Fatalf("inventory_floor exited %d: %s", exit, stderr)
		}
		return out
	}

	// An ecosystem the workflow has never heard of, taught by dropping a file.
	const anEcosystemSkill = "---\nname: stack-invented\ndescription: synthetic fixture\n---\n\n" +
		"<!-- iterion:manifests\n" + `["Brewfile.invented", "*.inventedproj"]` + "\n-->\n"

	t.Run("a shape only a skill declares is listed", func(t *testing.T) {
		ws := t.TempDir()
		writeSkills(t, bundleSkills(ws), map[string]string{"stack-invented.md": anEcosystemSkill})
		for path, body := range map[string]string{
			"Brewfile.invented": "one\n", "app.inventedproj": "two\n", "notes.txt": "three\n",
		} {
			if err := os.WriteFile(filepath.Join(ws, path), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		gittest.Run(t, ws, "init", "-q", "-b", "main")
		gittest.Run(t, ws, "config", "user.email", "t@example.com")
		gittest.Run(t, ws, "config", "user.name", "t")
		gittest.Run(t, ws, "config", "commit.gpgsign", "false")
		gittest.Run(t, ws, "add", "Brewfile.invented", "app.inventedproj", "notes.txt")
		gittest.Run(t, ws, "commit", "-qm", "fixture")
		sha := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))

		out := runFloor(t, ws, t.TempDir(), sha)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("the floor refused: %s", assessmentString(t, out, "reason"))
		}
		listing := assessmentString(t, out, "listing")
		for _, want := range []string{"Brewfile.invented", "app.inventedproj"} {
			if !strings.Contains(listing, want) {
				t.Errorf("the listing handed to the survey does not carry %q, whose shape only a "+
					"skill declares — the ecosystem would need a DSL edit:\n%s", want, listing)
			}
		}
		// And only those: the manifest section of the listing is what the
		// skills declare, not every file in the tree.
		section := listing[strings.Index(listing, "BUILD / PACKAGING MANIFESTS"):]
		if end := strings.Index(section, "\n\n"); end > 0 {
			section = section[:end]
		}
		if strings.Contains(section, "notes.txt") {
			t.Errorf("the floor listed a file no skill declares as a manifest:\n%s", section)
		}
	})

	t.Run("no manifest block anywhere is a refusal, not an empty listing", func(t *testing.T) {
		ws := t.TempDir()
		writeSkills(t, bundleSkills(ws), map[string]string{
			"stack-mute.md": "---\nname: stack-mute\ndescription: declares no shapes\n---\n"})
		if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, ws, "init", "-q", "-b", "main")
		gittest.Run(t, ws, "config", "user.email", "t@example.com")
		gittest.Run(t, ws, "config", "user.name", "t")
		gittest.Run(t, ws, "config", "commit.gpgsign", "false")
		gittest.Run(t, ws, "add", "go.mod")
		gittest.Run(t, ws, "commit", "-qm", "fixture")
		sha := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))

		out := runFloor(t, ws, t.TempDir(), sha)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a bundle declaring no manifest shape produced a listing with no manifest in " +
				"it — the survey would read a repository that builds itself")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "defect in the bundle") {
			t.Errorf("the refusal does not say whose defect it is: %s", assessmentString(t, out, "reason"))
		}
	})

	// And the shipped bundle covers what it claims to: the two stacks it ships
	// skills for are both discoverable from the listing alone.
	t.Run("the shipped skills declare the shapes their stacks are detected by", func(t *testing.T) {
		skills := map[string]string{}
		for _, name := range []string{"assessment-survey.md", "stack-go.md", "stack-node.md"} {
			body, err := os.ReadFile(filepath.Join("assessment", "skills", name))
			if err != nil {
				t.Fatal(err)
			}
			skills[name] = string(body)
		}
		ws := t.TempDir()
		writeSkills(t, bundleSkills(ws), skills)
		for path, body := range map[string]string{
			"go.mod": "module x\n", "package.json": "{}\n", "Dockerfile": "FROM scratch\n",
		} {
			if err := os.WriteFile(filepath.Join(ws, path), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		gittest.Run(t, ws, "init", "-q", "-b", "main")
		gittest.Run(t, ws, "config", "user.email", "t@example.com")
		gittest.Run(t, ws, "config", "user.name", "t")
		gittest.Run(t, ws, "config", "commit.gpgsign", "false")
		gittest.Run(t, ws, "add", "go.mod", "package.json", "Dockerfile")
		gittest.Run(t, ws, "commit", "-qm", "fixture")
		sha := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))

		listing := assessmentString(t, runFloor(t, ws, t.TempDir(), sha), "listing")
		for _, want := range []string{"go.mod", "package.json", "Dockerfile"} {
			if !strings.Contains(listing, want) {
				t.Errorf("the bundle ships a skill for the stack %q proves and does not declare its "+
					"shape:\n%s", want, listing)
			}
		}
	})
}

// The lines of an accented file must reach the published count. The floor's
// failure here is SILENT: a C-quoted key matches no first-party prefix in
// `measure`, so the file simply is not counted and nothing goes red.
func TestAssessmentFloorCountsAccentedPaths(t *testing.T) {
	requireAssessmentTools(t)
	ws := t.TempDir()
	writeSkills(t, bundleSkills(ws), map[string]string{
		"assessment-survey.md": mustRead(t, filepath.Join("assessment", "skills", "assessment-survey.md")),
	})
	for path, body := range map[string]string{
		"src/café.txt": "one\ntwo\nthree\n", "src/plain.txt": "one\n", "go.mod": "module x\n",
	} {
		full := filepath.Join(ws, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sha := commitAll(t, ws)

	out, exit, stderr := assessmentRun(t, "inventory_floor", map[string]string{
		"{{vars.workspace_dir}}":     ws,
		"{{vars.scratch_dir}}":       t.TempDir(),
		"{{vars.bundle_skills_dir}}": bundleSkills(ws),
		"{{input.base_sha}}":         sha,
	}, nil)
	if exit != 0 {
		t.Fatalf("inventory_floor exited %d: %s", exit, stderr)
	}
	body, err := os.ReadFile(assessmentString(t, out, "floor_path"))
	if err != nil {
		t.Fatal(err)
	}
	var floor map[string]any
	if err := json.Unmarshal(body, &floor); err != nil {
		t.Fatal(err)
	}
	perPath, _ := floor["lines_by_path"].(map[string]any)
	if _, ok := perPath["src/café.txt"]; !ok {
		t.Fatalf("the floor recorded the accented file under a C-quoted key, so `measure` will "+
			"never match it against the declared perimeter and its lines drop out in silence: %v",
			keysOf(perPath))
	}
}

// NO EXECUTABLE COMES OUT OF THE ASSESSED REPOSITORY. The interpreter lookup
// used to fall back on `<workspace>/.devbox/nix/profile/default/bin/<name>` —
// the audited checkout's own devbox profile — and then RAN it with the run's
// environment. That is the defect this bundle refuses for the skills
// themselves, left open for the binary that executes them.
func TestAssessmentInterpreterIsNeverTakenFromTheAssessedRepository(t *testing.T) {
	requireAssessmentTools(t)
	skill := strings.Replace(aStackSkill, `"interpreter":"python3"`,
		`"interpreter":"assessment-planted-interpreter"`, 1)
	if skill == aStackSkill {
		t.Fatal("the mutation did not apply")
	}
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": skill})

	// What a repository under assessment can commit: an executable at exactly
	// the path the fallback probed.
	planted := filepath.Join(ws, ".devbox", "nix", "profile", "default", "bin")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(ws, "the-planted-binary-ran")
	body := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(planted, "assessment-planted-interpreter"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	out := runExtractors(t, ws, t.TempDir(), []map[string]any{
		{"id": "synth", "evidence": "a", "supported": true}})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the runner executed a binary committed by the repository under assessment, with " +
			"this run's environment")
	}
	if assessmentBool(t, out, "ok") {
		t.Fatal("an interpreter absent from PATH resolved anyway")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "on no PATH") {
		t.Errorf("the refusal does not say where it looked: %s", assessmentString(t, out, "reason"))
	}
}

// THE ARTEFACT IS NOT THE MEASUREMENT: an extractor output that carries none
// of the facts its skill declares it emits counted as coverage landed, and the
// measure it promised published as a zero.
func TestAssessmentCoverageGateRefusesAnOutputThatDeliversNoPromisedFact(t *testing.T) {
	requireAssessmentTools(t)
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill})
	scratch := t.TempDir()
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}

	out := runExtractors(t, ws, scratch, stacks)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the runner refused: %s", assessmentString(t, out, "reason"))
	}
	// The extractor "ran" and wrote well-formed JSON carrying NO fact.
	target := filepath.Join(scratch, "synth-counts.json")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(original, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, has := parsed["facts"]; !has {
		t.Fatal("the fixture skill's output carries no facts mapping to strip")
	}
	parsed["facts"] = map[string]any{}
	stripped, _ := json.Marshal(parsed)
	if err := os.WriteFile(target, stripped, 0o644); err != nil {
		t.Fatal(err)
	}

	survey := writeSurvey(t, t.TempDir(), "deadbeef", stacks, nil)
	health := runHealth(t, ws, scratch, survey, out)
	if assessmentBool(t, health, "ok") {
		t.Fatal("an output delivering none of its promised facts counted as coverage")
	}
	reason := assessmentString(t, health, "reason")
	if !strings.Contains(reason, "entrypoints") || !strings.Contains(reason, "synth") {
		t.Errorf("the refusal does not name the stack and the missing fact: %s", reason)
	}
	if assessmentString(t, health, "code") != "COVERAGE_VOID" {
		t.Errorf("code = %q, want COVERAGE_VOID", assessmentString(t, health, "code"))
	}
}

// THE MEASURED FALSE POSITIVE: the method-per-verb shape is also the shape of
// the calls that CONSUME routes. http.Get( and resp.Header.Get( counted as
// route registrations, and the extractor's total answered a question nobody
// asked. Proven on the SHIPPED skill, executed against a real tree.
func TestAssessmentGoRouteRegexSkipsClientCalls(t *testing.T) {
	requireAssessmentTools(t)
	skill, err := os.ReadFile(filepath.Join("assessment", "skills", "stack-go.md"))
	if err != nil {
		t.Fatal(err)
	}
	ws := stackWorkspace(t, map[string]string{"stack-go.md": string(skill)})
	gittest.Run(t, ws, "init", "-q", "-b", "main")
	gittest.Run(t, ws, "config", "user.email", "t@example.com")
	gittest.Run(t, ws, "config", "user.name", "t")
	gittest.Run(t, ws, "config", "commit.gpgsign", "false")

	write := func(rel, body string) {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/app\n\ngo 1.21\n")
	write("main.go", "package main\n"+
		"\n"+
		"func fetch() {\n"+
		"\thttp.Get(\"https://api.invalid\")\n"+
		"\tresp.Header.Get(\"Content-Type\")\n"+
		"}\n"+
		"\n"+
		"func routes(r *mux) {\n"+
		"\tr.Get(\"/\")\n"+
		"}\n")
	gittest.Run(t, ws, "add", "-A")
	gittest.Run(t, ws, "commit", "-qm", "the commit under assessment")
	sha := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))

	scratch := t.TempDir()
	out := runExtractorsAt(t, ws, scratch, []map[string]any{
		{"id": "go", "evidence": "go.mod", "supported": true},
	}, sha)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("the shipped extractor refused a pinned pass: %s", assessmentString(t, out, "reason"))
	}
	body, err := os.ReadFile(filepath.Join(scratch, "go-entrypoints.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Facts map[string]any `json:"facts"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if got := document.Facts["entrypoints"]; got != float64(1) {
		t.Fatalf("entrypoints = %v, want 1 — the two client calls are not route registrations:\n%s",
			got, string(body))
	}
}
