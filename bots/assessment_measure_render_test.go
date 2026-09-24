package bots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The published size is a ratio to a SYNTHETIC anchor, cut into bands by a
// versioned profile. These tests pin the three things that make it honest: the
// letter never travels without its profile, there IS no letter outside the
// profile's domain, and a declaration counted twice moves the letter — which
// is why the declaration lint refuses one.

// measureWorkspace builds a workspace carrying the bundle's own measurement
// profile, mirrored where the runtime puts it.
func measureWorkspace(t *testing.T) string {
	t.Helper()
	profile, err := os.ReadFile("assessment/skills/measurement-profile.md")
	if err != nil {
		t.Fatal(err)
	}
	return stackWorkspace(t, map[string]string{"measurement-profile.md": string(profile)})
}

// writeFloor materialises a floor measurement the way inventory_floor does.
func writeFloor(t *testing.T, scratch string, lines map[string]int) string {
	t.Helper()
	perPath := map[string]any{}
	for path, count := range lines {
		perPath[path] = count
	}
	document := map[string]any{
		"base_sha": "deadbeefdeadbeef", "files": len(lines), "lines_by_path": perPath,
		"binary": []string{}, "oversized": []string{}, "by_extension": map[string]any{},
		"top_level": []string{"src"}, "manifests": []string{}, "ci_files": []string{},
		"ci_present": false, "commits": 42, "commits_recent_180d": 7,
		"history_can_testify": true, "tags_reachable": []string{}, "tags_in_clone": 0,
		"tag_dates": []string{}, "secret_literal_hits": 0, "secret_literal_keys": []string{},
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scratch, "floor.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func measure(t *testing.T, ws, scratch, surveyPath, floorPath string) map[string]any {
	t.Helper()
	return measureWithCoverage(t, ws, scratch, surveyPath, floorPath, "[]", "[]", "false")
}

// measureWithCoverage hands the measurement what the coverage gate
// established, the way the workflow does. The published gap is the
// deterministic layer's, never the agent's flag alone.
func measureWithCoverage(t *testing.T, ws, scratch, surveyPath, floorPath,
	unsupported, covered, degraded string) map[string]any {
	t.Helper()
	out, exit, stderr := assessmentRun(t, "measure", map[string]string{
		"{{vars.workspace_dir}}":     ws,
		"{{vars.scratch_dir}}":       scratch,
		"{{vars.profile_path}}":      "",
		"{{vars.bundle_skills_dir}}": bundleSkills(ws),
		"{{input.base_sha}}":         "deadbeefdeadbeef",
		"{{input.survey_path}}":      surveyPath,
		"{{input.floor_path}}":       floorPath,
	}, map[string]string{
		"{{input.extractor_outputs}}":  "[]",
		"{{input.stacks_unsupported}}": unsupported,
		"{{input.stacks_covered}}":     covered,
		"{{input.coverage_degraded}}":  degraded,
		"{{input.coverage_missing}}":   "[]",
	})
	if exit != 0 {
		t.Fatalf("measure exited %d: %s", exit, stderr)
	}
	return out
}

// a repository with source, entrypoints, deployables and systems: inside the
// public profile's declared domain.
func inDomainSurvey(deployables int) []map[string]any {
	declarations := []map[string]any{
		{"id": "src-tree", "kind": "first_party", "path": "src"},
		{"id": "relational-store", "kind": "system", "identity": "relational-store",
			"path": "config/store.ini"},
		{"id": "queue-broker", "kind": "system", "identity": "queue-broker",
			"path": "config/broker.ini"},
		// Thirty route registrations, declared as the count they are: the same
		// unit the extractors emit and the profile's anchor is written in.
		{"id": "http-surface", "kind": "entrypoint", "count": 30, "path": "src/routes.txt"},
	}
	for i := 0; i < deployables; i++ {
		declarations = append(declarations, map[string]any{
			"id": "service-" + string(rune('a'+i)), "kind": "deployable",
			"identity": "service-" + string(rune('a'+i)),
			"path":     "deploy/service-" + string(rune('a'+i)) + ".yaml",
		})
	}
	return declarations
}

func floorLines(total int) map[string]int {
	lines := map[string]int{}
	per := total / 10
	for i := 0; i < 10; i++ {
		lines["src/file"+string(rune('a'+i))+".txt"] = per
	}
	return lines
}

func TestAssessmentSizePublishesItsProfileOrNoLetterAtAll(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)
	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}

	t.Run("inside the domain, the band names its profile", func(t *testing.T) {
		survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, inDomainSurvey(2))
		out := measure(t, ws, scratch, survey, floor)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("measure refused a complete survey: %s", assessmentString(t, out, "reason"))
		}
		if !assessmentBool(t, out, "in_domain") {
			t.Fatalf("a full application was judged outside the profile's domain: %s",
				assessmentString(t, out, "reason"))
		}
		if assessmentString(t, out, "profile_id") == "" || assessmentString(t, out, "profile_version") == "" {
			t.Fatal("the measurement published no profile identity — a letter without its profile is a false quotation")
		}
		band := assessmentString(t, out, "size")
		if band == "" || band == "not-applicable" {
			t.Fatalf("size = %q inside the domain", band)
		}
		facts := assessmentString(t, out, "facts")
		if !strings.Contains(facts, "relative to profile") {
			t.Errorf("the published band fact does not carry its profile: %s", facts)
		}
	})

	t.Run("outside the domain, there is no letter", func(t *testing.T) {
		// A library: source, deployables and systems, and no entrypoint at all.
		// The geometric mean would drive the index to zero and size it the
		// smallest band, in silence. The scale says instead that it does not
		// apply.
		declarations := []map[string]any{
			{"id": "src-tree", "kind": "first_party", "path": "src"},
			{"id": "relational-store", "kind": "system", "identity": "relational-store",
				"path": "config/store.ini"},
			{"id": "service-a", "kind": "deployable", "identity": "service-a",
				"path": "deploy/service-a.yaml"},
		}
		survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, declarations)
		out := measure(t, ws, scratch, survey, floor)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("measure refused outright instead of reporting out of domain: %s",
				assessmentString(t, out, "reason"))
		}
		if assessmentBool(t, out, "in_domain") {
			t.Fatal("a repository with zero entrypoints was judged inside the domain")
		}
		if assessmentString(t, out, "size") != "not-applicable" {
			t.Fatalf("size = %q outside the domain, want not-applicable — the smallest band would be a lie with a shape",
				assessmentString(t, out, "size"))
		}
		// The raw measurements still land: "not applicable" is an answer, not a
		// refusal to measure.
		facts := assessmentString(t, out, "facts")
		if !strings.Contains(facts, "metric.first_party_lines") {
			t.Errorf("the raw measurements were dropped along with the letter: %s", facts)
		}
	})
}

// THE measurement behind the declaration lint. One deployable declared four
// times, the tree untouched: the index moves, and on the boundary the letter
// moves with it. This is why a duplicate is REFUSED rather than noted.
func TestAssessmentADeclarationCountedTwiceMovesThePublishedSize(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)
	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}

	once := measure(t, ws, scratch,
		writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, inDomainSurvey(1)), floor)
	quadruple := measure(t, ws, scratch,
		writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, inDomainSurvey(4)), floor)

	first, _ := once["index"].(float64)
	second, _ := quadruple["index"].(float64)
	if second <= first {
		t.Fatalf("declaring the same artefact more times did not move the index (%v -> %v) — "+
			"if it cannot move the number, the declaration lint is guarding nothing", first, second)
	}
	t.Logf("one deployable declared 1x vs 4x, tree untouched: index %.3f -> %.3f, size %q -> %q",
		first, second, assessmentString(t, once, "size"), assessmentString(t, quadruple, "size"))
}

// The renderer substitutes factual assertions by identifier and REFUSES one it
// does not know. That is the whole mechanism: a digit detector was the earlier
// form and does not work — "two majors behind" carries no digit, and a lexical
// filter rejects a legitimate cross-reference.
func TestAssessmentRenderRefusesAFactNobodyMeasured(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)
	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}
	survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, inDomainSurvey(2))
	measured := measure(t, ws, scratch, survey, floor)
	factsPath := assessmentString(t, measured, "facts_path")

	render := func(t *testing.T, state string) map[string]any {
		t.Helper()
		out, exit, stderr := assessmentRun(t, "render", map[string]string{
			"{{vars.workspace_dir}}":    ws,
			"{{vars.out_dir}}":          "docs/assessment",
			"{{input.facts_path}}":      factsPath,
			"{{input.state_judgement}}": state,
			"{{input.plan_judgement}}":  "The programme faces [[fact:stack.detected]].",
			"{{input.open_questions}}":  "- who owns the [[fact:metric.systems]] decision?",
		}, nil)
		if exit != 0 {
			t.Fatalf("render exited %d: %s", exit, stderr)
		}
		return out
	}

	t.Run("a known identifier is substituted", func(t *testing.T) {
		out := render(t, "The repository carries [[fact:metric.first_party_lines]] today.")
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("render refused prose citing a measured fact: %s", assessmentString(t, out, "reason"))
		}
		body, err := os.ReadFile(filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md"))
		if err != nil {
			t.Fatalf("no document was written: %v", err)
		}
		if strings.Contains(string(body), "[[fact:") {
			t.Fatal("a placeholder survived into the document")
		}
		if !strings.Contains(string(body), "line of first-party source") &&
			!strings.Contains(string(body), "lines of first-party source") {
			t.Fatalf("the measured text was not substituted:\n%s", body)
		}
	})

	t.Run("an identifier nobody measured stops the run", func(t *testing.T) {
		out := render(t, "The repository is [[fact:metric.invented_by_the_agent]] behind.")
		if assessmentBool(t, out, "ok") {
			t.Fatal("prose citing a fact identifier the measurement never produced was published")
		}
		reason := assessmentString(t, out, "reason")
		if !strings.Contains(reason, "metric.invented_by_the_agent") {
			t.Errorf("the refusal does not name the unknown identifier: %s", reason)
		}
		if assessmentString(t, out, "code") != "RENDER_REFUSED" {
			t.Errorf("code = %q, want RENDER_REFUSED", assessmentString(t, out, "code"))
		}
	})
}

// The published gap is the UNION of what the runner could not cover and what
// the survey declared. Either alone is smaller than the gap that exists: the
// agent does not know which skills this bundle ships, and the runner does not
// know a stack the agent could not cover conceptually.
//
// The measured consequence: a survey that declares every stack supported
// published "stacks NOT covered: none" while the deterministic layer knew its
// whole stack had no extractor.
func TestAssessmentPublishedCoverageIsTheUnionNotTheAgentsFlag(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)
	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	// The agent's flag says everything is fine.
	stacks := []map[string]any{{"id": "confident", "evidence": "a", "supported": true}}
	survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, inDomainSurvey(2))

	// The deterministic layer knows there is no extractor for it.
	out := measureWithCoverage(t, ws, scratch, survey, floor,
		`[{"stack": "confident", "reason": "no stack-confident.md in this bundle"}]`, `[]`, "true")
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("measure refused: %s", assessmentString(t, out, "reason"))
	}
	facts := assessmentString(t, out, "facts")
	if !strings.Contains(facts, "confident") {
		t.Fatalf("the published gap does not name the stack the runner could not cover — it is "+
			"reading the agent's flag:\n%s", facts)
	}
	if !strings.Contains(facts, "stack.coverage — the coverage of this measurement is DEGRADED") {
		t.Errorf("the coverage gate's `degraded` verdict never reaches the document:\n%s", facts)
	}
}

// TWO SCALES UNDER ONE METRIC. The anchor is written in route registrations
// and every extractor emits them; a declaration standing for one FILE answered
// 40 or 1 for the same forty routes depending on the layout, and the two
// numbers land in different bands.
//
// The declaration carries the count now, so the layout cannot move the letter.
func TestAssessmentEntrypointsAreCountedInOneUnit(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)
	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}

	base := []map[string]any{
		{"id": "src-tree", "kind": "first_party", "path": "src"},
		{"id": "relational-store", "kind": "system", "identity": "relational-store", "path": "config/store.ini"},
		{"id": "queue-broker", "kind": "system", "identity": "queue-broker", "path": "config/broker.ini"},
		{"id": "service-a", "kind": "deployable", "identity": "service-a", "path": "deploy/a.yaml"},
		{"id": "service-b", "kind": "deployable", "identity": "service-b", "path": "deploy/b.yaml"},
	}

	// Forty routes in one file.
	oneFile := append(append([]map[string]any{}, base...), map[string]any{
		"id": "http-surface", "kind": "entrypoint", "count": 40, "path": "src/routes.txt"})
	// The same forty routes, laid out four files of ten.
	fourFiles := append([]map[string]any{}, base...)
	for i, name := range []string{"alpha", "beta", "gamma", "delta"} {
		fourFiles = append(fourFiles, map[string]any{
			"id": "http-" + name, "kind": "entrypoint", "count": 10,
			"path": "src/routes-" + string(rune('a'+i)) + ".txt"})
	}

	compact := measure(t, ws, scratch, writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, oneFile), floor)
	spread := measure(t, ws, scratch, writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, fourFiles), floor)

	if compact["index"] != spread["index"] {
		t.Fatalf("the same forty routes measured %v laid out in one file and %v in four — the "+
			"metric is in files on one side and in registrations on the other",
			compact["index"], spread["index"])
	}
	if !strings.Contains(assessmentString(t, compact, "facts"), "route registrations") {
		t.Errorf("the published metric does not say what unit it is in:\n%s",
			assessmentString(t, compact, "facts"))
	}
}

// THE EXCLUSION RATE IS A FACT. The partition makes a survey account for every
// top-level entry; it does not make the account honest, and the cheapest route
// to a smaller project is a larger `excluded`. A reader who sees the rate can
// ask why; a reader who sees a band alone cannot.
func TestAssessmentPublishesWhatThePerimeterLeftOut(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)
	scratch := t.TempDir()
	lines := floorLines(20000)
	lines["vendor/dep.txt"] = 60000
	floor := writeFloor(t, scratch, lines)
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}

	declarations := append(inDomainSurvey(2), map[string]any{
		"id": "vendored", "kind": "excluded", "path": "vendor", "note": "vendored"})
	out := measure(t, ws, scratch, writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, declarations), floor)
	facts := assessmentString(t, out, "facts")
	if !strings.Contains(facts, "perimeter.exclusion_rate") {
		t.Fatalf("the measurement publishes no exclusion rate:\n%s", facts)
	}
	if !strings.Contains(facts, "75%") {
		t.Errorf("the exclusion rate is not the measured one (60000 of 80000 lines):\n%s", facts)
	}
	if !strings.Contains(facts, "`vendor`") {
		t.Errorf("the largest exclusion is not named:\n%s", facts)
	}
}

// renderJudgement runs the renderer over three judgement fields.
func renderJudgement(t *testing.T, ws, factsPath, state, plan, questions string) map[string]any {
	t.Helper()
	out, exit, stderr := assessmentRun(t, "render", map[string]string{
		"{{vars.workspace_dir}}":    ws,
		"{{vars.out_dir}}":          "docs/assessment",
		"{{input.facts_path}}":      factsPath,
		"{{input.state_judgement}}": state,
		"{{input.plan_judgement}}":  plan,
		"{{input.open_questions}}":  questions,
	}, nil)
	if exit != 0 {
		t.Fatalf("render exited %d: %s", exit, stderr)
	}
	return out
}

// SUBSTITUTING BY IDENTIFIER IS HALF THE MECHANISM. It keeps a figure the
// agent invented out of the document; it does nothing about a figure TYPED
// beside the placeholders, and "4 200 tests" or "98%" reads on the page
// exactly like a measurement. And a fact that is a bare scalar can be given a
// false context by the sentence built round it.
func TestAssessmentRenderRefusesATypedFigureAndAMisusedFact(t *testing.T) {
	requireAssessmentTools(t)
	ws := measureWorkspace(t)
	scratch := t.TempDir()
	floor := writeFloor(t, scratch, floorLines(20000))
	stacks := []map[string]any{{"id": "synth", "evidence": "a", "supported": true}}
	survey := writeSurvey(t, t.TempDir(), "deadbeefdeadbeef", stacks, inDomainSurvey(2))
	factsPath := assessmentString(t, measure(t, ws, scratch, survey, floor), "facts_path")

	plan := "The order follows [[fact:metric.deployables]]."
	questions := "- who arbitrates [[fact:metric.systems]]?"

	for _, tc := range []struct{ name, state, wants string }{
		{"a typed count", "The suite carries 4 200 tests today.", "measured by nobody"},
		{"a typed proportion", "Roughly 98% of the surface is covered.", "measured by nobody"},
		{"a typed version", "The runtime is on 2.9 and unsupported.", "measured by nobody"},

		{"a block citing no fact at all",
			"The repository is in reasonable shape and the risk is moderate.", "cite no measured fact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderJudgement(t, ws, factsPath, tc.state, plan, questions)
			if assessmentBool(t, out, "ok") {
				t.Fatalf("published prose containing %s", tc.name)
			}
			if !strings.Contains(assessmentString(t, out, "reason"), tc.wants) {
				t.Errorf("the refusal does not say why (%q): %s", tc.wants,
					assessmentString(t, out, "reason"))
			}
			if code := assessmentString(t, out, "code"); code != "RENDER_REFUSED" {
				t.Errorf("code = %q, want RENDER_REFUSED", code)
			}
		})
	}

	// The engine's own placeholder form. Its literal never appears in this
	// file either: the harness substitutes a Python EXPRESSION, so the two
	// braces only ever meet at run time — the same reason the renderer
	// assembles its pattern from pieces.
	t.Run("the engine's placeholder form is refused, not rendered", func(t *testing.T) {
		out, exit, stderr := assessmentRun(t, "render", map[string]string{
			"{{vars.workspace_dir}}":   ws,
			"{{vars.out_dir}}":         "docs/assessment",
			"{{input.facts_path}}":     factsPath,
			"{{input.plan_judgement}}": plan,
			"{{input.open_questions}}": questions,
		}, map[string]string{
			"{{input.state_judgement}}": `"The repository carries " + "{" + "{fact:metric.first_party_lines}}."`,
		})
		if exit != 0 {
			t.Fatalf("render exited %d: %s", exit, stderr)
		}
		if assessmentBool(t, out, "ok") {
			t.Fatal("a placeholder in the engine's own brace form was rendered as literal text " +
				"into a signed document, with no identifier ever checked")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "not the form") {
			t.Errorf("the refusal does not say what is wrong: %s", assessmentString(t, out, "reason"))
		}
	})

	// The legitimate shapes stay legal: a cross-reference, and an ordered list.
	t.Run("a cross-reference and an ordered list are not figures", func(t *testing.T) {
		out := renderJudgement(t, ws, factsPath,
			"The measurements in [[ref:§2]] are what this reading rests on, and "+
				"[[fact:metric.first_party_lines]] is the one that dominates.\n\n"+
				"1. the perimeter\n2. the order\n", plan, questions)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a guard that refuses the legitimate case gets worked around: %s",
				assessmentString(t, out, "reason"))
		}
		body, err := os.ReadFile(filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "§2") {
			t.Errorf("the cross-reference did not render as its own text:\n%s", body)
		}
		if strings.Contains(string(body), "[[ref:") {
			t.Error("a reference placeholder survived into the document")
		}
	})

	// A FACT IN A FALSE CONTEXT. The size index used to substitute as a bare
	// "1.25", so a sentence built round it published a measured number
	// certifying something nobody measured. A fact that names itself cannot be
	// borrowed that way.
	t.Run("a fact carries its own noun", func(t *testing.T) {
		out := renderJudgement(t, ws, factsPath,
			"The scan found [[fact:size.index]] critical vulnerabilities.", plan, questions)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("render refused prose citing a known fact: %s", assessmentString(t, out, "reason"))
		}
		body, err := os.ReadFile(filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "an index of") {
			t.Fatalf("the index substituted as a bare scalar — the sentence round it decides what "+
				"the number counts:\n%s", body)
		}
	})
}
