package bots

import (
	"strings"
	"testing"
)

// The survey's declarations are an INPUT TO AN ARITHMETIC whose result is
// published. Measured next door, in assessment_measure_render_test.go:
// declaring ONE deployable four times — repository untouched, evidence
// unchanged, every consistency check green — moves the published index from
// 0.658 to 0.931, and the band with it wherever a repository sits near one.
//
// So declaration_lint refuses rather than warns, and these tests pin each
// refusal to an example it MUST reject. Every one of them was run with its
// guard removed first: see the falsification record in the PR.

func declarationFixture(t *testing.T) (dir, sha string) {
	t.Helper()
	return synthRepo(t, map[string]string{
		"src/app/handler.txt":  "route alpha\nroute beta\n",
		"src/app/service.txt":  "service body\n",
		"src/worker/main.txt":  "worker body\n",
		"tests/unit/case.txt":  "a test\n",
		"third_party/dep.txt":  "vendored\n",
		"deploy/service.yaml":  "kind: Deployment\nname: alpha\n",
		"README.md":            "# fixture\n",
		"config/datastore.ini": "engine = relational\n",
	})
}

func lintDeclarations(t *testing.T, dir, sha string, declarations []map[string]any) map[string]any {
	t.Helper()
	stacks := []map[string]any{{"id": "synthetic", "evidence": "README.md", "supported": false,
		"reason": "fixture stack, no extractor skill"}}
	surveyPath := writeSurvey(t, t.TempDir(), sha, stacks, declarations)
	out, exit, stderr := assessmentRun(t, "declaration_lint",
		map[string]string{
			"{{vars.workspace_dir}}": dir,
			"{{input.base_sha}}":     sha,
			"{{input.survey_path}}":  surveyPath,
		}, nil)
	if exit != 0 {
		t.Fatalf("declaration_lint exited %d: %s", exit, stderr)
	}
	return out
}

// A complete, honest survey of the fixture: every top-level entry claimed,
// every path present, no artefact declared twice. This is the CONTROL — a
// guard that cannot go green on a good input is not a guard, it is a wall.
func wholeSurvey() []map[string]any {
	return []map[string]any{
		{"id": "src-app", "kind": "first_party", "path": "src"},
		{"id": "tests-unit", "kind": "tests", "path": "tests"},
		{"id": "vendored-deps", "kind": "excluded", "path": "third_party", "note": "vendored"},
		{"id": "deploy-manifests", "kind": "excluded", "path": "deploy", "note": "deployment descriptors"},
		{"id": "repo-docs", "kind": "excluded", "path": "README.md", "note": "documentation"},
		{"id": "repo-config", "kind": "excluded", "path": "config", "note": "configuration"},
		{"id": "service-alpha", "kind": "deployable", "path": "deploy/service.yaml", "pattern": "kind: Deployment"},
		{"id": "relational-store", "kind": "system", "path": "config/datastore.ini", "pattern": "engine\\s*="},
	}
}

func TestAssessmentDeclarationLintAcceptsACompleteSurvey(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)
	out := lintDeclarations(t, dir, sha, wholeSurvey())
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("a complete, verifiable survey was refused: %s", assessmentString(t, out, "reason"))
	}
	if got := out["verified"]; got != float64(len(wholeSurvey())) {
		t.Fatalf("verified = %v, want %d", got, len(wholeSurvey()))
	}
}

// THE measured failure: one artefact, declared four times. The lint must
// refuse it, because every reader downstream would count it four times and
// nothing in the tree would contradict them.
func TestAssessmentDeclarationLintRefusesTheSameArtefactDeclaredTwice(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)
	declarations := wholeSurvey()
	for _, suffix := range []string{"bis", "ter", "quater"} {
		declarations = append(declarations, map[string]any{
			"id": "service-alpha-" + suffix, "kind": "deployable",
			"path": "deploy/service.yaml", "pattern": "kind: Deployment",
		})
	}
	out := lintDeclarations(t, dir, sha, declarations)
	if assessmentBool(t, out, "ok") {
		t.Fatal("one deployable declared FOUR times was accepted — the published size would count it four times")
	}
	reason := assessmentString(t, out, "reason")
	for _, suffix := range []string{"bis", "ter", "quater"} {
		if !strings.Contains(reason, "service-alpha-"+suffix) {
			t.Errorf("the refusal does not name the duplicate %q: %s", suffix, reason)
		}
	}
}

func TestAssessmentDeclarationLintRefusesADuplicateID(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)
	declarations := append(wholeSurvey(), map[string]any{
		"id": "service-alpha", "kind": "system", "path": "config/datastore.ini",
	})
	out := lintDeclarations(t, dir, sha, declarations)
	if assessmentBool(t, out, "ok") {
		t.Fatal("two declarations under one id were accepted — a reader would take the first and a verifier the last")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "declared twice") {
		t.Errorf("refusal does not say the id is declared twice: %s", assessmentString(t, out, "reason"))
	}
}

// A declaration whose evidence is not in the tree is a CLAIM. The whole
// architecture rests on the lint being able to re-derive every declaration
// from the pinned commit; one it cannot re-derive is the hole.
func TestAssessmentDeclarationLintRefusesEvidenceThatIsNotThere(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)

	t.Run("a path that does not exist", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
			"id": "phantom-service", "kind": "deployable", "path": "deploy/absent.yaml",
		}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a deployable whose file is not in the tree was accepted")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "phantom-service") {
			t.Errorf("refusal does not name it: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("a pattern that matches nothing", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
			"id": "phantom-queue", "kind": "system", "path": "config/datastore.ini",
			"pattern": "broker\\s*=",
		}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a system whose pattern matches nothing in its own evidence was accepted")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "matches nothing") {
			t.Errorf("refusal does not say the pattern matched nothing: %s", assessmentString(t, out, "reason"))
		}
	})
}

// A subtree claimed twice leaves its lines counted twice or not at all, and no
// reader downstream can tell which happened.
func TestAssessmentDeclarationLintRefusesOverlappingSubtrees(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)
	out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
		"id": "src-app-again", "kind": "first_party", "path": "src/app",
	}))
	if assessmentBool(t, out, "ok") {
		t.Fatal("a first_party subtree nested inside another first_party subtree was accepted")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "claimed twice") {
		t.Errorf("refusal does not say the files are claimed twice: %s", assessmentString(t, out, "reason"))
	}
}

// Nesting in the OTHER direction is how a perimeter is carved and must stay
// legal: a guard that refuses the legitimate case would be worked around, and
// a worked-around guard measures nothing.
func TestAssessmentDeclarationLintAllowsTestsCarvedOutOfFirstParty(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := synthRepo(t, map[string]string{
		"src/app/handler.txt":    "route alpha\n",
		"src/app/tests/case.txt": "a test\n",
		"src/vendor/dep.txt":     "vendored\n",
		"README.md":              "# fixture\n",
		"deploy/service.yaml":    "kind: Deployment\n",
	})
	out := lintDeclarations(t, dir, sha, []map[string]any{
		{"id": "src-tree", "kind": "first_party", "path": "src"},
		{"id": "src-tests", "kind": "tests", "path": "src/app/tests"},
		{"id": "src-vendored", "kind": "excluded", "path": "src/vendor", "note": "vendored"},
		{"id": "repo-docs", "kind": "excluded", "path": "README.md", "note": "documentation"},
		{"id": "deploy-dir", "kind": "excluded", "path": "deploy", "note": "deployment descriptors"},
	})
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("carving tests and vendored code out of a first_party subtree was refused: %s",
			assessmentString(t, out, "reason"))
	}
}

// OMISSION is the failure no declaration can be refused for: an artefact never
// declared has no declaration to reject, nothing goes red, and the document
// simply describes a smaller project. The only mechanical handle is
// completeness of the partition — so the partition is checked.
func TestAssessmentDeclarationLintRefusesAnUnclaimedTopLevelEntry(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)
	partial := []map[string]any{}
	for _, declaration := range wholeSurvey() {
		if declaration["id"] == "repo-config" {
			continue // the subtree holding the datastore config: never claimed
		}
		if declaration["id"] == "relational-store" {
			continue
		}
		partial = append(partial, declaration)
	}
	out := lintDeclarations(t, dir, sha, partial)
	if assessmentBool(t, out, "ok") {
		t.Fatal("a top-level entry claimed by no declaration was accepted — that is the omission the lint exists to surface")
	}
	reason := assessmentString(t, out, "reason")
	if !strings.Contains(reason, "config") {
		t.Errorf("the refusal does not name the unclaimed entry: %s", reason)
	}
}

func TestAssessmentDeclarationLintRefusesAnUnknownKind(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)
	out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
		"id": "mystery", "kind": "whatever", "path": "README.md",
	}))
	if assessmentBool(t, out, "ok") {
		t.Fatal("a declaration of an unknown kind was accepted — every reader would skip it silently")
	}
}
