package ast_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

var provenanceFixture = map[string]string{
	"main.bot":        "dsl: 2\nimport \"lib/schemas.bot\"\nimport \"lib/nodes.bot\"\n\n## the main's comment\n\nvars:\n  goal: string\n\nsecrets:\n  token: \"${TOKEN}\"\n\nagent a:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"d\"\n  input: s\n\nworkflow w:\n  entry: a\n  a -> b\n  b -> done\n",
	"lib/schemas.bot": "schema s:\n  ok: bool\n\nvars:\n  depth: int\n\npresets:\n  fast:\n    depth: 1\n",
	"lib/nodes.bot":   "## the fragment's comment\n\nprompt p:\n  Hello.\n\n## on the agent\nagent b:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"e\"\n  system: p\n  output: s\n\nsubbot child:\n  source: \"kids/k.bot\"\n",
}

// carriers walks a raw document and returns, for every object that is a
// declaration, a block or a block entry, the path to it and its "file" —
// so a carrier that lost its provenance shows as an empty value, never as
// an absent path the assertion would not know to look for.
func carriers(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	blocks := map[string]string{"vars": "fields", "presets": "entries", "attachments": "fields", "secrets": "fields"}
	// nested records the comments a declaration or a block carries: they
	// are carriers too, and provenance has to reach them where they are.
	nested := func(prefix string, obj map[string]any) {
		cs, ok := obj["comments"].([]any)
		if !ok {
			return
		}
		for i, c := range cs {
			if co, ok := c.(map[string]any); ok {
				out[prefix+".comments["+itoa(i)+"]"] = stringOf(co["file"])
			}
		}
	}
	for key, v := range doc {
		switch vv := v.(type) {
		case []any:
			for i, e := range vv {
				if obj, ok := e.(map[string]any); ok {
					out[key+"["+itoa(i)+"]"] = stringOf(obj["file"])
					if key != "comments" {
						nested(key+"["+itoa(i)+"]", obj)
					}
				}
			}
		case map[string]any:
			inner, isBlock := blocks[key]
			if !isBlock {
				continue
			}
			out[key] = stringOf(vv["file"])
			nested(key, vv)
			if entries, ok := vv[inner].([]any); ok {
				for i, e := range entries {
					if obj, ok := e.(map[string]any); ok {
						out[key+"."+inner+"["+itoa(i)+"]"] = stringOf(obj["file"])
					}
				}
			}
		}
	}
	return out
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func itoa(i int) string { return strconv.Itoa(i) }

// TestProvenanceIsOnEveryCarrier: the document of a merged unit names, on
// every declaration, on each keyed block and each of its entries, and on
// every comment, the file it came from — the fragment's for what the
// fragment declared, the main's for the rest.
func TestProvenanceIsOnEveryCarrier(t *testing.T) {
	u := unit.LoadMap(provenanceFixture, "main.bot")
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	raw, err := ast.MarshalFileWithProvenance(u.Merged, "")
	if err != nil {
		t.Fatal(err)
	}
	got := carriers(t, raw)
	want := map[string]string{
		"agents[0]": "main.bot", "agents[1]": "lib/nodes.bot",
		"prompts[0]": "lib/nodes.bot", "schemas[0]": "lib/schemas.bot", "subbots[0]": "lib/nodes.bot",
		"workflows[0]": "main.bot",
		// A comment is a carrier too — at the file's head when it leads
		// no declaration, on the declaration it was written around
		// otherwise (#1282).
		"comments[0]": "main.bot", "comments[1]": "lib/nodes.bot", "agents[1].comments[0]": "lib/nodes.bot",
		"vars": "main.bot", "vars.fields[0]": "main.bot", "vars.fields[1]": "lib/schemas.bot",
		"secrets": "main.bot", "secrets.fields[0]": "main.bot",
		"presets": "lib/schemas.bot", "presets.entries[0]": "lib/schemas.bot",
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: file %q, want %q (carriers: %v)", k, got[k], w, keys)
		}
	}
	for k, v := range got {
		if v == "" {
			t.Errorf("%s carries no file", k)
		}
	}
}

// TestProvenanceIsReadBack: a document that came with provenance parses
// into an AST whose spans name their files, on declarations and on block
// entries alike — what a save routes on.
func TestProvenanceIsReadBack(t *testing.T) {
	u := unit.LoadMap(provenanceFixture, "main.bot")
	raw, err := ast.MarshalFileWithProvenance(u.Merged, "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.Agents[0].Span.Start.File != "main.bot" || f.Agents[1].Span.Start.File != "lib/nodes.bot" || f.Agents[1].Span.End.File != "lib/nodes.bot" {
		t.Fatalf("agents read back from %q and %q", f.Agents[0].Span.Start.File, f.Agents[1].Span.Start.File)
	}
	if f.Vars.Span.Start.File != "main.bot" || f.Vars.Fields[1].Span.Start.File != "lib/schemas.bot" || f.Presets.Span.Start.File != "lib/schemas.bot" {
		t.Fatalf("blocks read back from %q, %q, %q", f.Vars.Span.Start.File, f.Vars.Fields[1].Span.Start.File, f.Presets.Span.Start.File)
	}
	if f.Comments[1].Span.Start.File != "lib/nodes.bot" || f.Subbots[0].Span.Start.File != "lib/nodes.bot" {
		t.Fatalf("comment and subbot read back from %q and %q", f.Comments[1].Span.Start.File, f.Subbots[0].Span.Start.File)
	}
	// A comment carried by a declaration reads its file back too.
	if len(f.Agents[1].Comments) != 1 || f.Agents[1].Comments[0].Span.Start.File != "lib/nodes.bot" {
		t.Fatalf("the agent's comments read back as %+v", f.Agents[1].Comments)
	}
	// Without provenance, nothing is invented.
	plain, err := ast.UnmarshalFile(mustMarshal(t, u.Merged))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Agents[1].Span.Start.File != "" {
		t.Fatalf("a document without provenance read a file %q into a span", plain.Agents[1].Span.Start.File)
	}
}

// TestTheTransportCarriesNoProvenance: MarshalFile — what travels to a
// runner, what the studio's parse endpoint returns — has no "file" key
// anywhere, so it stays byte-identical to what it always was.
func TestTheTransportCarriesNoProvenance(t *testing.T) {
	u := unit.LoadMap(provenanceFixture, "main.bot")
	raw := mustMarshal(t, u.Merged)
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if paths := keysNamed(doc, "file", ""); len(paths) > 0 {
		t.Fatalf("the transport carries provenance at %v", paths)
	}
	if paths := keysNamed(mustUnmarshalAny(t, mustMarshalWithProvenance(t, u.Merged)), "file", ""); len(paths) == 0 {
		t.Fatal("the walk finds no file key even on a document with provenance: the check is inert")
	}
}

// TestProvenanceIsRelativeToTheRoot: a unit read from disk names its
// files by absolute path; the document names them from the unit's root,
// with slashes, so a save routes on the key a files map would use.
func TestProvenanceIsRelativeToTheRoot(t *testing.T) {
	root := t.TempDir()
	for rel, src := range provenanceFixture {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u := unit.LoadDir(filepath.Join(root, "main.bot"))
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	raw, err := ast.MarshalFileWithProvenance(u.Merged, u.Root)
	if err != nil {
		t.Fatal(err)
	}
	got := carriers(t, raw)
	if got["agents[0]"] != "main.bot" || got["agents[1]"] != "lib/nodes.bot" {
		t.Fatalf("files named %q and %q, want paths from the root", got["agents[0]"], got["agents[1]"])
	}
}

func mustMarshal(t *testing.T, f *ast.File) []byte {
	t.Helper()
	raw, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustMarshalWithProvenance(t *testing.T, f *ast.File) []byte {
	t.Helper()
	raw, err := ast.MarshalFileWithProvenance(f, "")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustUnmarshalAny(t *testing.T, raw []byte) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// keysNamed lists the paths of every object key named name in a decoded
// JSON value.
func keysNamed(v any, name, path string) []string {
	var out []string
	switch vv := v.(type) {
	case map[string]any:
		for k, e := range vv {
			if k == name {
				out = append(out, path+"."+k)
			}
			out = append(out, keysNamed(e, name, path+"."+k)...)
		}
	case []any:
		for i, e := range vv {
			out = append(out, keysNamed(e, name, path+"["+itoa(i)+"]")...)
		}
	}
	return out
}
