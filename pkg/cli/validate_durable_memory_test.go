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
