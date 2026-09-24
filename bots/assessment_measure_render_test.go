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
		"{{vars.workspace_dir}}": ws,
		"{{vars.scratch_dir}}":   scratch,
		"{{vars.profile_path}}":  "",
		"{{input.base_sha}}":     "deadbeefdeadbeef",
		"{{input.survey_path}}":  surveyPath,
		"{{input.floor_path}}":   floorPath,
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
			"{{input.plan_judgement}}":  "The programme is judged elsewhere.",
			"{{input.open_questions}}":  "- who owns the datastore decision?",
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
