package bots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// survey_write is the seam between the agent's answer and the lint's judgement.
// It exists so there IS one: between what an agent returned and what a gate
// reads there must be a file, and the file is what gets judged.
//
// Two things it owes. The shape is checked HERE and nowhere else, so every
// reader past it may assume a list of mappings — a reader that re-guesses the
// shape will guess differently from its neighbour. And the perimeter lands IN
// THE TREE: a published figure whose perimeter is not committed beside it
// cannot be recomputed by anyone.

func surveyWrite(t *testing.T, ws string, stacks, declarations any, notes string) map[string]any {
	t.Helper()
	stacksJSON, err := json.Marshal(stacks)
	if err != nil {
		t.Fatal(err)
	}
	declarationsJSON, err := json.Marshal(declarations)
	if err != nil {
		t.Fatal(err)
	}
	out, exit, stderr := assessmentRun(t, "survey_write",
		map[string]string{
			"{{vars.workspace_dir}}": ws,
			"{{vars.survey_path}}":   ".modernize/survey.json",
			"{{input.base_sha}}":     "deadbeefdeadbeef",
			"{{input.notes}}":        notes,
		},
		map[string]string{
			"{{input.stacks}}":       string(stacksJSON),
			"{{input.declarations}}": string(declarationsJSON),
		})
	if exit != 0 {
		t.Fatalf("survey_write exited %d: %s", exit, stderr)
	}
	return out
}

func TestAssessmentSurveyWriteLandsThePerimeterInTheTree(t *testing.T) {
	requireAssessmentTools(t)
	ws := t.TempDir()
	stacks := []map[string]any{{"id": "SYNTH", "evidence": "README.md", "supported": true}}
	declarations := []map[string]any{{"id": "src-tree", "kind": "first_party", "path": "src"}}

	out := surveyWrite(t, ws, stacks, declarations, "nothing could not be established")
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("a well-formed survey was refused: %s", assessmentString(t, out, "reason"))
	}

	body, err := os.ReadFile(filepath.Join(ws, ".modernize", "survey.json"))
	if err != nil {
		t.Fatalf("the perimeter did not land in the tree: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("the survey on disk is not JSON: %v", err)
	}
	if document["base_sha"] != "deadbeefdeadbeef" {
		t.Errorf("the survey does not carry the commit it describes: %v", document["base_sha"])
	}
	// The id is canonicalised once, here: two readers lower-casing it
	// independently is two chances to do it differently.
	written, _ := document["stacks"].([]any)
	first, _ := written[0].(map[string]any)
	if first["id"] != "synth" {
		t.Errorf("stack id = %v, want the canonical lower-case form", first["id"])
	}
}

func TestAssessmentSurveyWriteRefusesAnUnusableAnswer(t *testing.T) {
	requireAssessmentTools(t)

	t.Run("no stack at all", func(t *testing.T) {
		// A repository with no identifiable stack is a FINDING to write down,
		// not an empty list: past this node, an empty list is indistinguishable
		// from a repository nobody looked at.
		out := surveyWrite(t, t.TempDir(), []map[string]any{}, []map[string]any{}, "")
		if assessmentBool(t, out, "ok") {
			t.Fatal("a survey naming no stack at all was accepted — the coverage gate would then have nothing to be in lockstep with")
		}
		if code := assessmentString(t, out, "code"); code != "SURVEY_UNREADABLE" {
			t.Fatalf("code = %q, want SURVEY_UNREADABLE", code)
		}
	})

	t.Run("a stack with no evidence", func(t *testing.T) {
		out := surveyWrite(t, t.TempDir(),
			[]map[string]any{{"id": "synth", "supported": true}}, []map[string]any{}, "")
		if assessmentBool(t, out, "ok") {
			t.Fatal("a stack citing no evidence path was accepted — nothing downstream could re-verify it")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "evidence") {
			t.Errorf("the refusal does not say what is missing: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("a shape no later reader could parse", func(t *testing.T) {
		out := surveyWrite(t, t.TempDir(),
			[]map[string]any{{"id": "synth", "evidence": "a"}},
			[]any{"a declaration written as a bare string"}, "")
		if assessmentBool(t, out, "ok") {
			t.Fatal("a declaration that is not a mapping was accepted — every reader past this node assumes it is one")
		}
	})
}
