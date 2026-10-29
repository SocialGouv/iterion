package cli_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// durableStoreTool is the name of the durable memory store's write tool, read
// from the shared vocabulary the way the lint reads it: the member of
// DurableWriteCanonicals that toolcatalog's spelling table does not project
// onto any backend roster. Read rather than written so this fixture follows
// the vocabulary instead of pinning a word.
func durableStoreTool(t *testing.T) string {
	t.Helper()
	rows := toolcatalog.CanonicalRows()
	var out []string
	for _, canonical := range toolcatalog.DurableWriteCanonicals() {
		if _, projected := rows[canonical]; !projected {
			out = append(out, canonical)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("no member of toolcatalog.DurableWriteCanonicals names the durable store — the fixture below would assert nothing")
	}
	return out[0]
}

const durableMemoryBundleMain = `prompt sys:
  hello

agent nexie:
  backend: "claude_code"
  model: "anthropic/claude-sonnet-4-6"
  system: sys
  tools:   [read_file, glob, grep]

workflow w:
  entry: nexie
  nexie -> done
`

// TestRunValidate_SkillPromisingDurableMemoryWarns is the wire the ticket
// asks for, end to end: `iterion validate` on a bundle whose skill names the
// durable memory store's write tool, in a bundle whose only node declares a
// `tools:` list without a writer, emits C274 — naming the skill file — and
// validation still PASSES. The exit code is the part an operator feels, and
// ARB-98 settled that a skill authoring gap may not change it.
//
// It runs through the CLI rather than calling bundlelint directly because the
// page's BYTES are the CLI's half of the contract: bundlelint is I/O-free, so
// a scanBundleSkills that forgot the body would leave every unit witness in
// pkg/bundlelint green over a check that never sees a page.
func TestRunValidate_SkillPromisingDurableMemoryWarns(t *testing.T) {
	tool := durableStoreTool(t)
	dir := t.TempDir()
	writeBundle(t, dir, "name: probebot\n", durableMemoryBundleMain)
	skills := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	page := `---
name: session-continuity
description: How this bot keeps a knowledge tree across sessions.
---

# Session continuity

Call ` + "`" + tool + `(path, content)` + "`" + ` with YAML frontmatter; the
auto-index picks the file up on the next run.
`
	if err := os.WriteFile(filepath.Join(skills, "session-continuity.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := runValidateJSON(t, dir)
	if err != nil {
		t.Fatalf("a warning must not fail validation: %v", err)
	}
	if !res.Valid {
		t.Error("valid=false on a warning-only bundle — C274 changed the verdict")
	}
	if !hasCode(res.BundleDiagnostics, "C274") {
		t.Fatalf("no C274 bundle diagnostic: %v", res.BundleDiagnostics)
	}
	var found string
	for _, d := range res.BundleDiagnostics {
		if strings.Contains(d, "C274") {
			found = d
		}
	}
	for _, want := range []string{"skills/session-continuity.md", tool, `node "nexie"`} {
		if !strings.Contains(found, want) {
			t.Errorf("the diagnostic does not name %q: %s", want, found)
		}
	}

	// The scoped page — same bundle, same node — is quiet.
	quiet := `---
name: session-continuity
description: How this bot keeps a knowledge tree across sessions.
---

# Session continuity

This bot keeps nothing across sessions: the board is its durable record.
`
	if err := os.WriteFile(filepath.Join(skills, "session-continuity.md"), []byte(quiet), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = runValidateJSON(t, dir)
	if err != nil {
		t.Fatalf("validate the corrected bundle: %v", err)
	}
	if hasCode(res.BundleDiagnostics, "C274") {
		t.Errorf("the corrected page still draws C274: %v", res.BundleDiagnostics)
	}
}

// TestRunValidate_SkillInTheDirectoryFormIsJudged is the FORM half of
// iterion#2832: a bundle's skill may ship flat (`skills/<name>.md`) or as a
// directory (`skills/<name>/SKILL.md`), and the engine's own mirror documents
// both as inputs — runtime.mirrorBundleSkills copies a directory source
// through unchanged, runview parses `<dir>/SKILL.md`, and skilllib resolves
// both forms. A scan that skipped directory entries left the whole skill
// family of the lint (C231–C234 and C274) judging nothing there.
//
// The two forms carry the SAME bytes, so the only difference the diagnostics
// may show is the path.
func TestRunValidate_SkillInTheDirectoryFormIsJudged(t *testing.T) {
	tool := durableStoreTool(t)
	page := `---
name: session-continuity
description: How this bot keeps a knowledge tree across sessions.
---

# Session continuity

Call ` + "`" + tool + `(path, content)` + "`" + ` with YAML frontmatter; the
auto-index picks the file up on the next run.
`
	dir := t.TempDir()
	writeBundle(t, dir, "name: probebot\n", durableMemoryBundleMain)
	nested := filepath.Join(dir, "skills", "session-continuity")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "SKILL.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := runValidateJSON(t, dir)
	if err != nil {
		t.Fatalf("a warning must not fail validation: %v", err)
	}
	if !res.Valid {
		t.Error("valid=false on a warning-only bundle")
	}
	if !hasCode(res.BundleDiagnostics, "C274") {
		t.Fatalf("a skill in the directory form drew no C274 — the scan is blind to `skills/<name>/SKILL.md`: %v", res.BundleDiagnostics)
	}
	var found string
	for _, d := range res.BundleDiagnostics {
		if strings.Contains(d, "C274") {
			found = d
		}
	}
	// The PATH the operator must open, not the directory: the page is the
	// file inside it.
	for _, want := range []string{"skills/session-continuity/SKILL.md", tool, `node "nexie"`} {
		if !strings.Contains(found, want) {
			t.Errorf("the diagnostic does not name %q: %s", want, found)
		}
	}

	// The routability family is reachable there too, which is what makes this
	// a FORM fix rather than one check's: a directory page with no
	// frontmatter draws C231 and C232 exactly as a flat one does.
	orphan := filepath.Join(dir, "skills", "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "SKILL.md"), []byte("# Orphan\n\nNo frontmatter at all.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = runValidateJSON(t, dir)
	if err != nil {
		t.Fatalf("validate the bundle with a frontmatter-less directory page: %v", err)
	}
	for _, code := range []string{"C231", "C232"} {
		if !hasCode(res.BundleDiagnostics, code) {
			t.Errorf("a frontmatter-less page in the directory form drew no %s: %v", code, res.BundleDiagnostics)
		}
	}
}
