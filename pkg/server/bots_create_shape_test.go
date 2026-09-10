package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBotCreate_ShapeRendersTheShapeAndItsAnnexes: a `shape` in the POST
// body renders that gallery graph — not the single-agent default in
// silence — and the annex files the shape ships land in the bundle.
func TestBotCreate_ShapeRendersTheShapeAndItsAnnexes(t *testing.T) {
	srv, workdir := newBotCreateServer(t)
	rec := postBotCreate(t, srv, `{
		"slug": "mf",
		"instructions": "Write the thing.",
		"shape": "multi-file"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	dir := filepath.Join(workdir, "bots", "mf")
	src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "prompt mission:") || !strings.Contains(string(src), "system: mission") {
		t.Errorf("main.bot is not the multi-file shape (its prompts live in prompts/):\n%s", src)
	}
	for _, rel := range []string{"prompts/mission.md", "prompts/kickoff.md", "skills/house-style.md"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s missing: %v", rel, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(dir, "prompts", "mission.md"))
	if err != nil || !strings.Contains(string(body), "Write the thing.") {
		t.Errorf("prompts/mission.md does not carry the instructions: %q (%v)", body, err)
	}
}

// TestBotCreate_ShapeVarsMissingIs400: a var the shape's template
// references and the form dropped is refused by name as a client error —
// never met as a compiler diagnostic behind a 500.
func TestBotCreate_ShapeVarsMissingIs400(t *testing.T) {
	srv, _ := newBotCreateServer(t)
	rec := postBotCreate(t, srv, `{
		"slug": "fan",
		"instructions": "Review it.",
		"shape": "review-fanout",
		"vars": []
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "base") {
		t.Errorf("the refusal does not name the missing var: %s", rec.Body.String())
	}
}

// TestBotCreate_GeneratedWorkflowRefusedIs422: a mission whose text
// references a var nobody declared makes the generated workflow fail to
// compile — the operator's input, answered as 422 with the diagnostic.
func TestBotCreate_GeneratedWorkflowRefusedIs422(t *testing.T) {
	srv, _ := newBotCreateServer(t)
	rec := postBotCreate(t, srv, `{
		"slug": "typo",
		"instructions": "Report on {{vars.nope}}."
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("the response does not carry the diagnostic: %s", rec.Body.String())
	}
}
