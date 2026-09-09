package ir

import (
	"os"
	"path/filepath"
	"testing"
)

// The extractor must see a fence wherever markdown allows one — indented
// inside a list item, with its indentation stripped from the body — and must
// surface a malformed tag instead of dropping the fence. Both were blind
// spots of the first version: two indented fences in docs/sandbox.md were
// never checked, and a tag with a space would have vanished silently.
func TestDocSnippetExtractorSeesIndentedFencesAndBadTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	md := "Intro\n\n" +
		"1. Step one:\n\n" +
		"   ```iter fragment:workflow\n" +
		"   entry: plan\n" +
		"   sandbox: auto\n" +
		"   ```\n\n" +
		"- bullet:\n" +
		"  ```iter\n" +
		"  schema out:\n" +
		"    ok: bool\n" +
		"  ```\n\n" +
		"```iter fragment edges\n" +
		"a -> b\n" +
		"```\n"
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	snips := extractDocSnippets(t, path)
	if len(snips) != 3 {
		t.Fatalf("expected 3 fences (two indented, one with a bad tag), got %d: %+v", len(snips), snips)
	}
	if snips[0].tag != "fragment:workflow" || snips[0].body != "entry: plan\nsandbox: auto\n" {
		t.Errorf("indented fence 1 = tag %q body %q — indentation not stripped or tag lost", snips[0].tag, snips[0].body)
	}
	if snips[1].tag != "" || snips[1].body != "schema out:\n  ok: bool\n" {
		t.Errorf("indented fence 2 = tag %q body %q — nested indentation must survive, list indent must not", snips[1].tag, snips[1].body)
	}
	if snips[2].tag != "fragment edges" {
		t.Fatalf("bad tag = %q, want it captured verbatim so compileSnippet can refuse it", snips[2].tag)
	}
	if _, _, err := compileSnippet(snips[2]); err == nil {
		t.Error("a tag with a space must be refused, not treated as a policy")
	}
	// The de-indented fragment wraps and parses like a column-0 one.
	if pe, _, err := compileSnippet(snips[0]); err != nil || len(pe) > 0 {
		t.Errorf("indented fragment:workflow should parse once de-indented: err=%v parseErrs=%v", err, pe)
	}
}
