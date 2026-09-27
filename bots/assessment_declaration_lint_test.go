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
		{"id": "service-alpha", "kind": "deployable", "identity": "alpha", "path": "deploy/service.yaml", "pattern": "kind: Deployment"},
		{"id": "relational-store", "kind": "system", "identity": "relational-store", "path": "config/datastore.ini", "pattern": "engine\\s*="},
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
			"id": "service-alpha-" + suffix, "kind": "deployable", "identity": "alpha",
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
		"id": "service-alpha", "kind": "system", "identity": "another-store",
		"path": "config/datastore.ini",
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
			"id": "phantom-service", "kind": "deployable", "identity": "phantom",
			"path": "deploy/absent.yaml",
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
			"id": "phantom-queue", "kind": "system", "identity": "phantom-queue",
			"path": "config/datastore.ini", "pattern": "broker\\s*=",
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

// OMISSION IS A LEVER, and the partition is the only handle on it. A
// declaration of any kind used to mark its top-level root as accounted for, so
// ONE `entrypoint` on a file inside `src/` rendered the whole of `src/`
// claimed: everything else under it could go undeclared, and nothing would go
// red. Only the three kinds that partition may account for an entry, and only
// at the top level.
func TestAssessmentDeclarationLintPartitionsOnlyOnThePartitionKinds(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)

	t.Run("an entrypoint does not account for the subtree it sits in", func(t *testing.T) {
		partial := []map[string]any{}
		for _, declaration := range wholeSurvey() {
			if declaration["id"] == "src-app" {
				continue // the whole of src/, never claimed by a partition kind
			}
			partial = append(partial, declaration)
		}
		partial = append(partial, map[string]any{
			"id": "app-routes", "kind": "entrypoint", "count": 2, "pattern": "route",
			"path": "src/app/handler.txt",
		})
		out := lintDeclarations(t, dir, sha, partial)
		if assessmentBool(t, out, "ok") {
			t.Fatal("one entrypoint inside src/ accounted for the whole of src/ — every other " +
				"file under it could go undeclared and the published size would shrink in silence")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "src") {
			t.Errorf("the refusal does not name the unclaimed subtree: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("a partition declaration deeper than the top level accounts for itself", func(t *testing.T) {
		partial := []map[string]any{}
		for _, declaration := range wholeSurvey() {
			if declaration["id"] == "src-app" {
				declaration = map[string]any{"id": "src-app", "kind": "first_party", "path": "src/app"}
			}
			partial = append(partial, declaration)
		}
		out := lintDeclarations(t, dir, sha, partial)
		if assessmentBool(t, out, "ok") {
			t.Fatal("first_party on src/app accounted for all of src/ — src/worker is outside every declaration")
		}
	})
}

// IDENTITY IS THE KEY, not the path. A service described by its container
// file, its compose entry and its chart is ONE deployable with three proofs;
// counted by path it is three, and every consistency check stays green.
func TestAssessmentDeclarationLintDeduplicatesADeployableOnItsIdentity(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := synthRepo(t, map[string]string{
		"src/app/handler.txt":   "route alpha\n",
		"deploy/Dockerfile":     "FROM scratch\n# service: alpha\n",
		"deploy/compose.yaml":   "services:\n  alpha: {}\n  beta: {}\n",
		"deploy/chart/app.yaml": "kind: Deployment\nname: alpha\n",
		"README.md":             "# fixture\n",
	})
	base := []map[string]any{
		{"id": "src-tree", "kind": "first_party", "path": "src"},
		{"id": "repo-docs", "kind": "excluded", "path": "README.md", "note": "documentation"},
		{"id": "deploy-dir", "kind": "excluded", "path": "deploy", "note": "deployment descriptors"},
	}

	t.Run("three proofs of one service are one declaration too many", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(append([]map[string]any{}, base...),
			map[string]any{"id": "alpha-image", "kind": "deployable", "identity": "alpha",
				"path": "deploy/Dockerfile"},
			map[string]any{"id": "alpha-compose", "kind": "deployable", "identity": "alpha",
				"path": "deploy/compose.yaml"},
			map[string]any{"id": "alpha-chart", "kind": "deployable", "identity": "alpha",
				"path": "deploy/chart/app.yaml"}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("one service proved by three files counted as three deployables — a 41% lift " +
				"on that metric with the tree untouched")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "one artefact, one declaration") {
			t.Errorf("the refusal does not say why: %s", assessmentString(t, out, "reason"))
		}
	})

	// And the legitimate case stays legal: one file declaring two services.
	t.Run("two services in one file are two declarations", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(append([]map[string]any{}, base...),
			map[string]any{"id": "svc-alpha", "kind": "deployable", "identity": "alpha",
				"path": "deploy/compose.yaml", "pattern": "alpha"},
			map[string]any{"id": "svc-beta", "kind": "deployable", "identity": "beta",
				"path": "deploy/compose.yaml", "pattern": "beta"}))
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("one compose file declaring two services was refused: %s",
				assessmentString(t, out, "reason"))
		}
	})

	t.Run("a deployable with no identity has no key to be compared on", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(append([]map[string]any{}, base...),
			map[string]any{"id": "alpha-image", "kind": "deployable", "path": "deploy/Dockerfile"}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a deployable declaring no identity was accepted — it can only be deduplicated by path")
		}
		// ITS refusal, not the shape check beside it: an empty identity fails
		// the shape too, and a test that only asked for the word "identity"
		// could not tell the two guards apart.
		if !strings.Contains(assessmentString(t, out, "reason"), "declares no `identity`") {
			t.Errorf("the refusal does not name the missing field: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("an identity that is not a key", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(append([]map[string]any{}, base...),
			map[string]any{"id": "alpha-image", "kind": "deployable", "identity": "the alpha service",
				"path": "deploy/Dockerfile"}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("an identity written as prose was accepted — two spellings of one service would " +
				"not compare equal, and the dedup key would count it twice")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "lower-case words joined by dashes") {
			t.Errorf("the refusal does not say what an identity looks like: %s", assessmentString(t, out, "reason"))
		}
	})
}

// An entrypoint declaration must say HOW MANY. Every extractor in this bundle
// counts route registrations and the profile's anchor is written in them; a
// declaration standing for one FILE answers 40 or 1 for the same forty routes
// depending on the layout.
func TestAssessmentDeclarationLintRefusesAnEntrypointWithNoCount(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := declarationFixture(t)

	for _, tc := range []struct {
		name string
		decl map[string]any
	}{
		{"no count at all", map[string]any{
			"id": "app-routes", "kind": "entrypoint", "path": "src/app/handler.txt"}},
		{"a count of zero", map[string]any{
			"id": "app-routes", "kind": "entrypoint", "count": 0, "path": "src/app/handler.txt"}},
		{"a count that is not a number", map[string]any{
			"id": "app-routes", "kind": "entrypoint", "count": "several", "path": "src/app/handler.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := lintDeclarations(t, dir, sha, append(wholeSurvey(), tc.decl))
			if assessmentBool(t, out, "ok") {
				t.Fatal("an entrypoint declaration carrying no usable count was accepted — the " +
					"metric would be in files while the anchor is in route registrations")
			}
			if !strings.Contains(assessmentString(t, out, "reason"), "count") {
				t.Errorf("the refusal does not name the count: %s", assessmentString(t, out, "reason"))
			}
		})
	}

	t.Run("a declared count is read from its evidence", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
			"id": "app-routes", "kind": "entrypoint", "count": 2, "pattern": "route",
			"path": "src/app/handler.txt"}))
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("an entrypoint whose count matches its pattern was refused: %s",
				assessmentString(t, out, "reason"))
		}
	})

	t.Run("a declared count that diverges from its evidence is refused", func(t *testing.T) {
		// Changing only the declared figure — 2 to 12, the file untouched —
		// moved the published index by more than the whole survey earned:
		// the letter is a measurement, and this is where the invention dies.
		out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
			"id": "app-routes", "kind": "entrypoint", "count": 12, "pattern": "route",
			"path": "src/app/handler.txt"}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a figure the evidence contradicts was accepted")
		}
		reason := assessmentString(t, out, "reason")
		if !strings.Contains(reason, "12") || !strings.Contains(reason, "2") {
			t.Errorf("the refusal does not name both figures: %s", reason)
		}
	})

	t.Run("a counted declaration without the pattern that counts it is refused", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
			"id": "app-routes", "kind": "entrypoint", "count": 12, "path": "src/app/handler.txt"}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a figure nobody can re-derive was accepted")
		}
	})
}

// GIT C-QUOTES NON-ASCII PATHS. `ls-tree --name-only` emits
// `"src/caf\303\251.go"` for any name carrying a byte >= 0x80, and the
// neutralised environment every node here uses keeps that default. The quoted
// spelling then matches no declaration, and the partition gains a phantom
// top-level entry (`"src`) that nothing can ever claim — so the run dies with
// DECLARATIONS_REFUSED on a repository that is perfectly well surveyed.
//
// The same read in the floor fails silently instead: the quoted keys never
// match a first-party prefix, and the lines of every accented file drop out of
// the published count.
func TestAssessmentReadsAccentedPathsAsThemselves(t *testing.T) {
	requireAssessmentTools(t)
	dir, sha := synthRepo(t, map[string]string{
		"src/café.txt":       "accented\n",
		"src/naïve/main.txt": "accented directory\n",
		"src/plain.txt":      "plain\n",
		"README.md":          "# fixture\n",
	})

	t.Run("the declaration lint accepts a survey of an accented tree", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, []map[string]any{
			{"id": "src-tree", "kind": "first_party", "path": "src"},
			{"id": "repo-docs", "kind": "excluded", "path": "README.md", "note": "documentation"},
		})
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a complete survey of a tree holding an accented filename was refused: %s",
				assessmentString(t, out, "reason"))
		}
	})

	t.Run("a declaration citing the accented path is verified", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, []map[string]any{
			{"id": "src-tree", "kind": "first_party", "path": "src"},
			{"id": "repo-docs", "kind": "excluded", "path": "README.md", "note": "documentation"},
			{"id": "accented-entry", "kind": "entrypoint", "count": 1, "pattern": "accented", "path": "src/café.txt"},
		})
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a declaration citing an accented path was refused — the lint is reading the "+
				"C-quoted spelling: %s", assessmentString(t, out, "reason"))
		}
	})
}

// A FILE claim is a claim too: the nesting walk read only directories, so
// the same file declared under two kinds — or a first_party file inside an
// excluded subtree — passed with a contradictory perimeter.
func TestAssessmentDeclarationLintRefusesContradictoryFileClaims(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq", "bash")
	dir, sha := declarationFixture(t)

	t.Run("the same file under two kinds", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
			"id": "readme-again", "kind": "first_party", "path": "README.md"}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a file claimed excluded AND first_party was accepted — the perimeter contradicts itself")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "README.md") {
			t.Errorf("the refusal does not name the file: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("a first_party file inside an excluded subtree", func(t *testing.T) {
		out := lintDeclarations(t, dir, sha, append(wholeSurvey(), map[string]any{
			"id": "inside-vendored", "kind": "first_party", "path": "third_party/dep.txt"}))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a first_party file inside an excluded subtree was accepted — the file is claimed twice, or not at all")
		}
	})
}
