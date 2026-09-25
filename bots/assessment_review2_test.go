package bots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// aManifestDeclaringSkill carries the `iterion:manifests` block the floor
// refuses to run without: a bundle that declares no manifest shape anywhere is
// a defect in the bundle, and the floor says so rather than handing the survey
// an empty listing.
const aManifestDeclaringSkill = "---\nname: assessment-survey\ndescription: synthetic fixture\n---\n\n" +
	"<!-- iterion:manifests\n[\"*.py\", \"*.txt\", \"go.mod\"]\n-->\n"

// TWO READS OF ONE TREE MUST AGREE ON WHAT IS IN IT. The floor lists the tree
// with `ls-tree -r -l` and kept only `blob` entries; the declaration lint
// partitions over `ls-tree -r --name-only`, which also emits a submodule's
// gitlink (`160000 commit <oid> -`). So the survey was never shown a top-level
// entry the lint then demanded a declaration for, and the run died with
// DECLARATIONS_REFUSED on any repository carrying a submodule — the same class
// as the C-quoted path.
//
// Both directions on one bench: the entry reaches the listing, and a survey
// that claims it is accepted.
func TestAssessmentFloorListsASubmoduleTheLintWillDemand(t *testing.T) {
	requireAssessmentTools(t)

	inner, _ := synthRepo(t, map[string]string{"lib.txt": "lib\n"})
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill,
		"assessment-survey.md": aManifestDeclaringSkill})
	gittest.Run(t, ws, "init", "-q", "-b", "main")
	gittest.Run(t, ws, "config", "user.email", "t@example.com")
	gittest.Run(t, ws, "config", "user.name", "t")
	gittest.Run(t, ws, "config", "commit.gpgsign", "false")
	for rel, body := range map[string]string{"src/a.py": "code\n", "README.md": "doc\n"} {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gittest.Run(t, ws, "-c", "protocol.file.allow=always", "submodule", "add", "-q", inner, "vendor_sub")
	gittest.Run(t, ws, "add", "--", "src/a.py", "README.md", ".gitmodules", "vendor_sub")
	gittest.Run(t, ws, "commit", "-qm", "a tree carrying a submodule")
	sha := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))

	scratch := t.TempDir()
	floor, exit, stderr := assessmentRun(t, "inventory_floor", map[string]string{
		"{{vars.workspace_dir}}":     ws,
		"{{vars.scratch_dir}}":       scratch,
		"{{vars.bundle_skills_dir}}": bundleSkills(ws),
		"{{input.base_sha}}":         sha,
	}, nil)
	if exit != 0 {
		t.Fatalf("inventory_floor exited %d: %s", exit, stderr)
	}
	if !assessmentBool(t, floor, "ok") {
		t.Fatalf("the floor refused a repository with a submodule: %s", assessmentString(t, floor, "reason"))
	}
	if listing := assessmentString(t, floor, "listing"); !strings.Contains(listing, "vendor_sub") {
		t.Fatalf("the listing handed to the survey does not name the submodule, and the declaration "+
			"lint will refuse the run for not claiming it:\n%s", listing)
	}

	// And the survey CAN claim it: the lint accepts the gitlink declared
	// `excluded`, so the two reads agree.
	declarations := []map[string]any{
		{"id": "src-tree", "kind": "first_party", "path": "src"},
		{"id": "repo-docs", "kind": "excluded", "path": "README.md", "note": "documentation"},
		{"id": "modules-file", "kind": "excluded", "path": ".gitmodules", "note": "submodule pointer"},
		{"id": "vendored-sub", "kind": "excluded", "path": "vendor_sub", "note": "a submodule"},
	}
	lint := lintDeclarations(t, ws, sha, declarations)
	if !assessmentBool(t, lint, "ok") {
		t.Fatalf("a survey claiming every entry INCLUDING the submodule was refused: %s",
			assessmentString(t, lint, "reason"))
	}
}

// THE SURVEY IS TOLD WHICH STACK SKILLS THE BUNDLE SHIPS. The coverage gate
// refuses a run where the agent's `supported` flag and the runner's
// `stack-<id>.md` lookup disagree — and the agent had no channel for that
// fact: neither skill it loads enumerates the shipped ids, and the listing
// carried none. On any repository whose stack this bundle does not ship (or
// whose id is spelled `golang` rather than `go`), `supported: true` was a
// forced guess and the run died blaming the survey for it.
func TestAssessmentFloorTellsTheSurveyWhichStacksAreShipped(t *testing.T) {
	requireAssessmentTools(t)

	ws := stackWorkspace(t, map[string]string{
		"stack-go.md":              aStackSkill,
		"stack-node.md":            aStackSkill,
		"assessment-survey.md":     "---\nname: assessment-survey\n---\n\n<!-- iterion:manifests\n[\"*.txt\"]\n-->\n",
		"measurement-profile.md":   "---\nname: measurement-profile\n---\n",
		"not-a-stack-skill-at-all": "ignored: no .md suffix\n",
	})
	sha := initAssessedRepo(t, ws, map[string]string{"go.mod": "module example.test/app\n"})

	floor, exit, stderr := assessmentRun(t, "inventory_floor", map[string]string{
		"{{vars.workspace_dir}}":     ws,
		"{{vars.scratch_dir}}":       t.TempDir(),
		"{{vars.bundle_skills_dir}}": bundleSkills(ws),
		"{{input.base_sha}}":         sha,
	}, nil)
	if exit != 0 {
		t.Fatalf("inventory_floor exited %d: %s", exit, stderr)
	}
	if !assessmentBool(t, floor, "ok") {
		t.Fatalf("the floor refused: %s", assessmentString(t, floor, "reason"))
	}
	listing := assessmentString(t, floor, "listing")
	line := ""
	for _, candidate := range strings.Split(listing, "\n") {
		if strings.HasPrefix(candidate, "STACK EXTRACTOR SKILLS THIS BUNDLE SHIPS") {
			line = candidate
		}
	}
	if line == "" {
		t.Fatalf("the listing does not tell the survey which stacks are shipped, so `supported` is a "+
			"guess and the coverage gate refuses the run for it:\n%s", listing)
	}
	// The IDS, not the file names, and only the stack skills.
	for _, want := range []string{"go", "node"} {
		if !strings.Contains(line, want) {
			t.Errorf("the shipped-stack line does not name %q: %s", want, line)
		}
	}
	for _, unwanted := range []string{"stack-go.md", "measurement-profile", "assessment-survey"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("the shipped-stack line carries %q, which is not a stack id: %s", unwanted, line)
		}
	}
}

// A FLAT REPOSITORY HAS AN EXPRESSIBLE PERIMETER. `first_party` and `tests`
// took a directory only, and the sole directory of a flat tree is the root,
// whose empty path is refused — so source at the root had to be declared
// `excluded`, the measurement counted nothing, and the document published "0
// lines of first-party source" with a 100 % exclusion rate over a tree full of
// code. A withheld letter is a result; a published zero is a wrong figure.
func TestAssessmentAFlatRepositoryCanDeclareItsSource(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := synthRepo(t, map[string]string{
		"main.py":      "print(1)\n",
		"helpers.py":   "def h(): pass\n",
		"test_main.py": "def test(): pass\n",
		"README.md":    "# flat\n",
	})

	t.Run("source at the root is declared first_party, file by file", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, []map[string]any{
			{"id": "entry", "kind": "first_party", "path": "main.py"},
			{"id": "helpers", "kind": "first_party", "path": "helpers.py"},
			{"id": "unit-tests", "kind": "tests", "path": "test_main.py"},
			{"id": "repo-docs", "kind": "excluded", "path": "README.md", "note": "documentation"},
		})
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a flat repository could not declare its own source: %s", assessmentString(t, out, "reason"))
		}
	})

	// The measurement follows: what `first_party` claims is counted, so the
	// published figure is the source and not zero.
	t.Run("the measurement counts it", func(t *testing.T) {
		ws := measureWorkspace(t)
		scratch := t.TempDir()
		floor := writeFloor(t, scratch, map[string]int{
			"main.py": 12000, "helpers.py": 9000, "test_main.py": 400, "README.md": 40,
		})
		declarations := []map[string]any{
			{"id": "entry", "kind": "first_party", "path": "main.py"},
			{"id": "helpers", "kind": "first_party", "path": "helpers.py"},
			{"id": "unit-tests", "kind": "tests", "path": "test_main.py"},
			{"id": "repo-docs", "kind": "excluded", "path": "README.md", "note": "documentation"},
			{"id": "the-service", "kind": "deployable", "identity": "the-service", "path": "main.py"},
			{"id": "the-store", "kind": "system", "identity": "the-store", "path": "helpers.py"},
			{"id": "http-surface", "kind": "entrypoint", "count": 20, "path": "main.py"},
		}
		survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef",
			[]map[string]any{{"id": "synth", "evidence": "main.py", "supported": true}}, declarations)
		out := measure(t, ws, scratch, survey, floor)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("measure refused: %s", assessmentString(t, out, "reason"))
		}
		facts := assessmentString(t, out, "facts")
		if !strings.Contains(facts, "metric.first_party_lines — 21000 lines of first-party source\n") {
			t.Fatalf("the flat repository's own source was not counted as first-party:\n%s", facts)
		}
	})
}

// THE GATE PROBE MAY NOT REWRITE WHAT THIS RUN IS ABOUT TO PUBLISH. Everything
// the run produces — the contract this node has just parsed, its outcomes, the
// survey, the rendered documents — is still UNCOMMITTED when the probe runs, so
// a diff against HEAD cannot see a gate command rewriting one of them. The
// commit stages what is on disk, so the branch would carry a programme no lint
// ever read, under a message saying it was validated whole.
func TestAssessmentGateProbeRefusesAGateThatRewritesTheRunsOwnArtefacts(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")

	for _, tc := range []struct{ name, gate, wants string }{
		{"a gate rewriting the contract it was parsed from",
			`      - "bash -c 'printf \"\\nrewritten: true\\n\" >> .modernize/plan.yaml; exit 1'"`,
			".modernize/plan.yaml"},
		{"a gate rewriting the outcomes beside it",
			`      - "bash -c 'printf \"{}\" > .modernize/outcomes.json; exit 1'"`,
			".modernize/outcomes.json"},
		{"a gate rewriting a rendered document",
			`      - "bash -c 'printf \"forged\\n\" >> docs/assessment/00-state-of-the-repository.md; exit 1'"`,
			"00-state-of-the-repository.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`, tc.gate, 1)
			if contract == aGoodContract {
				t.Fatal("the mutation did not apply")
			}
			// THE ARTEFACTS ARE UNCOMMITTED, as they are at probe time in a real
			// run: the contract, its outcomes and the document are written into
			// the workspace and not staged. A fixture that committed them would
			// let `git diff HEAD` catch the rewrite, and this test would pass
			// without the guard it exists for.
			dir := uncommittedAssessmentRepo(t, contract)
			doc := filepath.Join(dir, "docs", "assessment", "00-state-of-the-repository.md")
			for _, path := range []string{".modernize/plan.yaml", ".modernize/outcomes.json", doc} {
				if strings.TrimSpace(gittest.Run(t, dir, "status", "--porcelain", "--", path)) == "" {
					t.Fatalf("%s is not uncommitted in the fixture, so `git diff HEAD` would catch a "+
						"rewrite of it and this test would prove nothing", path)
				}
			}
			out := lintContractWithDocuments(t, dir, aGoodBrief, []string{doc})
			if assessmentBool(t, out, "ok") {
				t.Fatalf("a gate that rewrote %s was accepted — the commit stages what is on disk, so "+
					"the branch would carry what no lint read", tc.wants)
			}
			reason := assessmentString(t, out, "reason")
			if !strings.Contains(reason, "WRITES to what it checks") || !strings.Contains(reason, tc.wants) {
				t.Errorf("the refusal does not name the rewrite: %s", reason)
			}
		})
	}

	// The neighbour that must stay green on the same bench: a gate that only
	// READS the contract, and leaves it as it found it.
	t.Run("a gate that reads the contract is a check", func(t *testing.T) {
		contract := strings.Replace(aGoodContract, `      - "bash ci/build.sh"`,
			`      - "bash -c 'grep -q \"version: 1\" .modernize/plan.yaml && exit 1'"`, 1)
		out := lintContractIn(t, uncommittedAssessmentRepo(t, contract), aGoodBrief)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a gate that only read the contract was refused: %s", assessmentString(t, out, "reason"))
		}
	})
}

// uncommittedAssessmentRepo is the workspace as the gate probe finds it: a
// repository whose history carries none of this run's artefacts, with the
// contract, its outcomes and a rendered document written and unstaged.
func uncommittedAssessmentRepo(t *testing.T, contract string) string {
	t.Helper()
	dir, _ := synthRepo(t, map[string]string{
		"README.md":   "# fixture\n",
		"ci/build.sh": "echo 'the build does not pass on this tree yet' >&2\nexit 1\n",
	})
	for rel, body := range map[string]string{
		".modernize/plan.yaml":                          contract,
		".modernize/outcomes.json":                      goodOutcomes,
		".modernize/survey.json":                        `{"version": 1}`,
		"docs/assessment/00-state-of-the-repository.md": "# State\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// lintContractWithDocuments runs the lint the way the workflow does once the
// state document exists: the paths come from the node that WROTE them.
func lintContractWithDocuments(t *testing.T, dir, brief string, documents []string) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(documents)
	if err != nil {
		t.Fatal(err)
	}
	out, exit, stderr := assessmentRun(t, "contract_lint", map[string]string{
		"{{vars.workspace_dir}}": dir,
		"{{vars.scratch_dir}}":   t.TempDir(),
		"{{vars.plan_path}}":     ".modernize/plan.yaml",
		"{{vars.survey_path}}":   ".modernize/survey.json",
	}, map[string]string{
		"{{input.brief}}":               briefJSON(t, brief),
		"{{input.documents}}":           string(encoded),
		"{{vars.gate_probe_timeout_s}}": gateProbeWall,
	})
	if exit != 0 {
		t.Fatalf("contract_lint exited %d: %s", exit, stderr)
	}
	return out
}

// A PUBLISHED FACT PLURALISES ITS OWN NOUN. `plural` appends an `s`, which is
// right only where the noun is the phrase's last word: two facts were phrases
// with a postmodifier, and the document published "21000 line of first-party
// sources" — the count on one noun and the plural on another, in prose an
// operator acts on. The helper refuses such a phrase without an explicit plural
// now, so the next one cannot be written silently.
func TestAssessmentPublishedFactsPluraliseTheirOwnNoun(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)

	read := func(t *testing.T, systems int) map[string]string {
		t.Helper()
		scratch := t.TempDir()
		declarations := []map[string]any{
			{"id": "src-tree", "kind": "first_party", "path": "src"},
			{"id": "http-surface", "kind": "entrypoint", "count": 30, "path": "src/routes.txt"},
			{"id": "service-a", "kind": "deployable", "identity": "service-a", "path": "deploy/a.yaml"},
		}
		for i := 0; i < systems; i++ {
			name := "store-" + string(rune('a'+i))
			declarations = append(declarations, map[string]any{
				"id": name, "kind": "system", "identity": name, "path": "config/" + name + ".ini"})
		}
		survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef",
			[]map[string]any{{"id": "synth", "evidence": "a", "supported": true}}, declarations)
		out := measure(t, ws, scratch, survey, writeFloor(t, scratch, floorLines(20000)))
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("measure refused: %s", assessmentString(t, out, "reason"))
		}
		facts := map[string]string{}
		for _, line := range strings.Split(assessmentString(t, out, "facts"), "\n") {
			if id, text, found := strings.Cut(line, " — "); found {
				facts[id] = text
			}
		}
		return facts
	}

	one, two := read(t, 1), read(t, 2)
	for _, tc := range []struct{ id, singular, plural string }{
		{"metric.systems", "1 distinct system the application talks to",
			"2 distinct systems the application talks to"},
	} {
		if got := one[tc.id]; got != tc.singular {
			t.Errorf("%s at one = %q, want %q", tc.id, got, tc.singular)
		}
		if got := two[tc.id]; got != tc.plural {
			t.Errorf("%s at two = %q, want %q", tc.id, got, tc.plural)
		}
	}
	if got := two["metric.first_party_lines"]; got != "20000 lines of first-party source" {
		t.Errorf("metric.first_party_lines = %q, want %q", got, "20000 lines of first-party source")
	}
	// The partition kinds may name a FILE now, so the noun may not say subtree.
	for _, id := range []string{"survey.first_party_subtrees", "survey.excluded_subtrees",
		"survey.tests_subtrees"} {
		if strings.Contains(two[id], "subtree") {
			t.Errorf("%s says %q, but a partition declaration may name a file", id, two[id])
		}
	}
}

// A GATE MAY ONLY REFUSE A CLAIM ON A FACT THE AGENT COULD OBSERVE — the same
// class as the shipped-stack ids, and here it was a SILENT DROP. A change
// policy takes a plain sentence or a mapping carrying `statement` (and the
// `pattern` that enforces it, which is the form the brief skill documents as
// the mechanism). The summary kept only the scalars, so the prompt said "none
// declared" over a policy that was declared, and `contract_lint` then refused a
// lot for announcing what the drafter had never been shown.
func TestAssessmentBriefSummaryCarriesEveryChangePolicy(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq")

	brief := `version: 1
objective: |
  A paragraph.
goals:
  - id: g1
    statement: "a goal"
owner: "the platform group"
permitted_changes:
  - statement: "dependency majors, when the behavioural net stays green"
forbidden_changes:
  - "a plain sentence"
  - statement: "the public interface of the reporting module"
    pattern: "public (API|interface)"
`
	out := readBrief(t, brief)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("a brief carrying both policy forms was refused: %s", assessmentString(t, out, "reason"))
	}
	summary := assessmentString(t, out, "summary")
	for _, want := range []string{
		"dependency majors, when the behavioural net stays green",
		"a plain sentence",
		"the public interface of the reporting module",
		"public (API|interface)",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary handed to the agents does not carry %q — the lint refuses a lot on a "+
				"policy the drafter was never shown:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "Permitted changes: none declared") {
		t.Errorf("the summary says no permitted change is declared while the brief declares one:\n%s", summary)
	}

	// The refusal, rather than a drop, for an entry nobody can render.
	t.Run("a policy entry with no statement is a refusal", func(t *testing.T) {
		out := readBrief(t, strings.Replace(brief,
			`  - statement: "the public interface of the reporting module"`,
			`  - pattern: "public (API|interface)"`, 1))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a forbidden change with no statement was accepted — it is invisible to the drafter " +
				"and enforced against it")
		}
		if code := assessmentString(t, out, "code"); code != "BRIEF_INCOMPLETE" {
			t.Fatalf("code = %q, want BRIEF_INCOMPLETE", code)
		}
	})
}

// THE CLASS BEHIND THE SUBMODULE: the floor and the declaration lint read the
// same tree with two different git invocations, and every entry one of them
// lists the other must list too. `ls-tree -l` carries a type field the floor
// filtered on; `--name-only` does not. Exercised over every object type a git
// tree can hold — a blob, an executable, a symlink and a gitlink.
func TestAssessmentTheFloorAndTheLintListTheSameTree(t *testing.T) {
	requireAssessmentTools(t)

	inner, _ := synthRepo(t, map[string]string{"lib.txt": "lib\n"})
	ws := stackWorkspace(t, map[string]string{"stack-synth.md": aStackSkill,
		"assessment-survey.md": aManifestDeclaringSkill})
	gittest.Run(t, ws, "init", "-q", "-b", "main")
	gittest.Run(t, ws, "config", "user.email", "t@example.com")
	gittest.Run(t, ws, "config", "user.name", "t")
	gittest.Run(t, ws, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(ws, "plain.txt"), []byte("a blob\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("plain.txt", filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, ws, "-c", "protocol.file.allow=always", "submodule", "add", "-q", inner, "sub")
	gittest.Run(t, ws, "add", "--", "plain.txt", "run.sh", "link.txt", ".gitmodules", "sub")
	gittest.Run(t, ws, "commit", "-qm", "one of every tree entry type")
	sha := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))

	scratch := t.TempDir()
	floor, exit, stderr := assessmentRun(t, "inventory_floor", map[string]string{
		"{{vars.workspace_dir}}":     ws,
		"{{vars.scratch_dir}}":       scratch,
		"{{vars.bundle_skills_dir}}": bundleSkills(ws),
		"{{input.base_sha}}":         sha,
	}, nil)
	if exit != 0 {
		t.Fatalf("inventory_floor exited %d: %s", exit, stderr)
	}
	if !assessmentBool(t, floor, "ok") {
		t.Fatalf("the floor refused: %s", assessmentString(t, floor, "reason"))
	}

	// What the floor TELLS the survey, read out of the artefact it wrote.
	var written struct {
		TopLevel []string `json:"top_level"`
	}
	raw, err := os.ReadFile(filepath.Join(scratch, "floor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, entry := range written.TopLevel {
		listed[strings.TrimSuffix(entry, "/")] = true
	}
	for _, want := range []string{"plain.txt", "run.sh", "link.txt", ".gitmodules", "sub"} {
		if !listed[want] {
			t.Errorf("the floor does not list %q, which the declaration lint will demand a declaration "+
				"for: two reads of one tree that do not agree on what is in it (top_level = %v)",
				want, written.TopLevel)
		}
	}

	// And the lint accepts a survey that claims exactly what the floor listed:
	// if either reader saw an entry the other did not, one of the two fails.
	declarations := []map[string]any{
		{"id": "the-source", "kind": "first_party", "path": "plain.txt"},
		{"id": "the-script", "kind": "excluded", "path": "run.sh", "note": "a shell script"},
		{"id": "the-link", "kind": "excluded", "path": "link.txt", "note": "a symlink"},
		{"id": "modules-file", "kind": "excluded", "path": ".gitmodules", "note": "submodule pointer"},
		{"id": "the-submodule", "kind": "excluded", "path": "sub", "note": "a submodule"},
	}
	lint := lintDeclarations(t, ws, sha, declarations)
	if !assessmentBool(t, lint, "ok") {
		t.Fatalf("a survey claiming every entry the floor listed was refused — the two readers disagree: %s",
			assessmentString(t, lint, "reason"))
	}
}
