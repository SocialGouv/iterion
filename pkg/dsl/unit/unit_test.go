package unit

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const mainSrc = "dsl: 2\nimport \"lib/schemas.bot\"\nimport \"lib/nodes.bot\"\n\nvars:\n  goal: string\n\nagent a:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"d\"\n  input: s\n\nworkflow w:\n  entry: a\n  a -> b\n  b -> done\n"

var fixture = map[string]string{
	"main.bot":        mainSrc,
	"lib/schemas.bot": "schema s:\n  ok: bool\n\nvars:\n  depth: int\n",
	// A fragment imports its sibling by the bare name: a path is relative to
	// the file that imports it, and the sibling is already in lib/.
	"lib/nodes.bot": "import \"schemas.bot\"\n\nagent b:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"a\\nb\"\n  output: s\n\nsubbot child:\n  source: \"kids/k.bot\"\n",
}

// The unit is the main plus every fragment its imports reach, once each,
// main first: the declarations of every kind are appended, the keyed
// blocks merge by key, the imports are gone from the merged file, and each
// file keeps its own profile — the fragment's string reads verbatim under
// its own profile 1 while the main is profile 2.
func TestAUnitMergesItsFragmentsMainFirst(t *testing.T) {
	u := LoadMap(fixture, "main.bot")
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	var rels []string
	for _, f := range u.Files {
		rels = append(rels, f.Rel)
	}
	if !reflect.DeepEqual(rels, []string{"main.bot", "lib/schemas.bot", "lib/nodes.bot"}) {
		t.Fatalf("files %v", rels)
	}
	m := u.Merged
	if m.Imports != nil || m.Profile != 2 || m.EffectiveProfile() != 2 {
		t.Fatalf("merged head: imports=%v profile=%d", m.Imports, m.Profile)
	}
	if len(m.Agents) != 2 || m.Agents[0].Name != "a" || m.Agents[1].Name != "b" || len(m.Schemas) != 1 || len(m.Subbots) != 1 || len(m.Workflows) != 1 {
		t.Fatalf("merged lists: agents=%d schemas=%d subbots=%d workflows=%d", len(m.Agents), len(m.Schemas), len(m.Subbots), len(m.Workflows))
	}
	if len(m.Vars.Fields) != 2 || m.Vars.Fields[0].Name != "goal" || m.Vars.Fields[1].Name != "depth" {
		t.Fatalf("merged vars: %+v", m.Vars.Fields)
	}
	if u.Files[2].Profile != 1 || m.Agents[1].Description != `a\nb` {
		t.Fatalf("the fragment did not keep its own profile: profile %d, description %q", u.Files[2].Profile, m.Agents[1].Description)
	}
	// The main's own AST is untouched by the merge.
	if len(u.Files[0].AST.Agents) != 1 || len(u.Files[0].AST.Imports) != 2 || len(u.Files[0].AST.Vars.Fields) != 1 {
		t.Fatalf("the main's AST was mutated: %+v", u.Files[0].AST)
	}
	if u.Digest == "" || len(u.Digest) != 64 {
		t.Fatalf("digest %q", u.Digest)
	}
}

// A subbot declared in a fragment is written against the fragment's
// directory; every host resolves a child against the unit's root, so the
// merged copy carries the root-relative source. The fragment's text stays.
func TestASubbotDeclaredInAFragmentIsMadeRootRelative(t *testing.T) {
	u := LoadMap(fixture, "main.bot")
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	if got := u.Merged.Subbots[0].Source; got != "lib/kids/k.bot" {
		t.Fatalf("merged subbot source %q", got)
	}
	if got := u.Files[2].AST.Subbots[0].Source; got != "kids/k.bot" {
		t.Fatalf("the fragment's own text changed: %q", got)
	}
	// One in the main stays as written.
	files := map[string]string{"main.bot": "subbot child:\n  source: \"kids/k.bot\"\n\nworkflow w:\n  entry: child\n  child -> done\n"}
	if got := LoadMap(files, "main.bot").Merged.Subbots[0].Source; got != "kids/k.bot" {
		t.Fatalf("the main's subbot source changed: %q", got)
	}
}

// The digest follows the files: a fragment edited, a fragment added, and
// nothing else.
func TestTheDigestFollowsEveryFileOfTheUnit(t *testing.T) {
	base := LoadMap(fixture, "main.bot").Digest
	edited := map[string]string{}
	for k, v := range fixture {
		edited[k] = v
	}
	edited["lib/schemas.bot"] = strings.Replace(edited["lib/schemas.bot"], "ok: bool", "ok: string", 1)
	if LoadMap(edited, "main.bot").Digest == base {
		t.Fatalf("a fragment edit did not change the digest")
	}
	same := map[string]string{}
	for k, v := range fixture {
		same[k] = v
	}
	same["lib/unrelated.bot"] = "agent z:\n  description: \"z\"\n" // not imported: not part of the unit
	if LoadMap(same, "main.bot").Digest != base {
		t.Fatalf("a file the unit does not reach changed the digest")
	}
}

// What the loader refuses, each at the import that led there: a fragment
// outside lib/, a missing one, a cycle; and a name declared in two files
// or a key declared twice, naming both places.
func TestTheLoaderNamesWhatItRefuses(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		code  parser.DiagCode
		msg   string
		file  string
		line  int
	}{
		{
			"outside lib",
			map[string]string{"main.bot": "import \"x.bot\"\n", "x.bot": "agent a:\n  description: \"d\"\n"},
			parser.DiagBadImportPath, "outside the bot's `lib/` directory", "main.bot", 1,
		},
		{
			"missing",
			map[string]string{"main.bot": "\n\nimport \"lib/x.bot\"\n"},
			parser.DiagImportUnreadable, "no such fragment (lib/x.bot)", "main.bot", 3,
		},
		{
			"cycle",
			map[string]string{"main.bot": "import \"lib/a.bot\"\n", "lib/a.bot": "import \"b.bot\"\n", "lib/b.bot": "import \"a.bot\"\n"},
			parser.DiagImportCycle, "main.bot → lib/a.bot → lib/b.bot → lib/a.bot", "lib/b.bot", 1,
		},
		{
			"a node in two files",
			map[string]string{"main.bot": "import \"lib/a.bot\"\nagent x:\n  description: \"d\"\n", "lib/a.bot": "\ntool x:\n  command: \"true\"\n"},
			parser.DiagDuplicateDecl, "declared in two files: here and at main.bot:2", "lib/a.bot", 2,
		},
		{
			"a var twice across files",
			map[string]string{"main.bot": "import \"lib/a.bot\"\nvars:\n  goal: string\n", "lib/a.bot": "vars:\n  goal: int\n"},
			parser.DiagDuplicateDecl, "var \"goal\" is declared twice: here and at main.bot:3", "lib/a.bot", 2,
		},
		{
			"a var twice in one block",
			map[string]string{"main.bot": "vars:\n  goal: string\n  goal: int\n"},
			parser.DiagDuplicateDecl, "var \"goal\" is declared twice: here and at main.bot:2", "main.bot", 3,
		},
		{
			"two workflows",
			map[string]string{"main.bot": "import \"lib/a.bot\"\nworkflow w:\n  entry: done\n", "lib/a.bot": "workflow v:\n  entry: done\n"},
			parser.DiagDuplicateDecl, "a unit has one workflow: \"v\" here and \"w\" at main.bot:2", "lib/a.bot", 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := LoadMap(c.files, "main.bot")
			var found *parser.Diagnostic
			for i := range u.Diagnostics {
				if u.Diagnostics[i].Code == c.code {
					found = &u.Diagnostics[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no %s in %v", c.code, u.Diagnostics)
			}
			if !strings.Contains(found.Message, c.msg) || found.File != c.file || found.Line != c.line {
				t.Fatalf("got %s at %s:%d %q, want %q at %s:%d", found.Code, found.File, found.Line, found.Message, c.msg, c.file, c.line)
			}
			if found.Hint == "" {
				t.Fatalf("the diagnostic carries no remedy")
			}
		})
	}
	// The same node twice in ONE file is the compiler's to report, not the
	// loader's: no E010 from here.
	u := LoadMap(map[string]string{"main.bot": "agent x:\n  description: \"d\"\n\ntool x:\n  command: \"true\"\n"}, "main.bot")
	for _, d := range u.Diagnostics {
		if d.Code == parser.DiagDuplicateDecl {
			t.Fatalf("a same-file duplicate was reported by the loader: %v", d)
		}
	}
}

// On disk the unit is read under the main's directory, every file parsed
// under its absolute path (so includes resolve beside their file); a
// fragment that is a symlink, or that resolves beyond the root through
// one, is not read and is named.
func TestLoadDirReadsUnderTheRootAndNoFurther(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, src := range fixture {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u := LoadDir(filepath.Join(root, "main.bot"))
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	if u.Root != root || u.Main != "main.bot" || u.Files[1].Name != filepath.Join(root, "lib", "schemas.bot") {
		t.Fatalf("root %q main %q name %q", u.Root, u.Main, u.Files[1].Name)
	}
	if LoadMap(fixture, "main.bot").Digest != u.Digest {
		t.Fatalf("the same files read from disk and from a map have different digests")
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "evil.bot"), []byte("agent z:\n  description: \"z\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "evil.bot"), filepath.Join(root, "lib", "link.bot")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.bot"), []byte("import \"lib/link.bot\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	u = LoadDir(filepath.Join(root, "main.bot"))
	var refused bool
	for _, d := range u.Diagnostics {
		if d.Code == parser.DiagImportUnreadable && strings.Contains(d.Message, "symlink") {
			refused = true
		}
	}
	if !refused || len(u.Files) != 1 {
		t.Fatalf("a symlinked fragment was read: %v (files %d)", u.Diagnostics, len(u.Files))
	}
	// A symlinked directory leaving the root is refused too.
	if err := os.Remove(filepath.Join(root, "lib", "link.bot")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "lib", "out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.bot"), []byte("import \"lib/out/evil.bot\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	u = LoadDir(filepath.Join(root, "main.bot"))
	refused = false
	for _, d := range u.Diagnostics {
		if d.Code == parser.DiagImportUnreadable {
			refused = true
		}
	}
	if !refused || len(u.Files) != 1 {
		t.Fatalf("a fragment through a symlinked directory was read: %v", u.Diagnostics)
	}
}

// A file that is not there, or that does not parse, is reported where it
// is — the main included.
func TestAMissingOrBrokenFileIsReportedInPlace(t *testing.T) {
	u := LoadMap(map[string]string{}, "main.bot")
	if len(u.Diagnostics) != 1 || u.Diagnostics[0].Code != parser.DiagImportUnreadable || u.Merged == nil {
		t.Fatalf("missing main: %v merged=%v", u.Diagnostics, u.Merged)
	}
	u = LoadMap(map[string]string{"main.bot": "import \"lib/a.bot\"\n", "lib/a.bot": "agent a:\n  bogus: 1\n"}, "main.bot")
	if !u.HasErrors() || u.Diagnostics[0].File != "lib/a.bot" {
		t.Fatalf("a fragment's parse error is not in its file: %v", u.Diagnostics)
	}
}

// TestLoadDirWithMainTakesTheDocumentAsMain: the studio launches a document
// that may differ from the file on disk. The unit is then the document's
// text as its main — parsed under the name the caller gives — with the
// fragments read beside the main on disk, under their own paths.
func TestLoadDirWithMainTakesTheDocumentAsMain(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, src := range fixture {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// On disk the main imports nothing; the document does.
	if err := os.WriteFile(filepath.Join(root, "main.bot"), []byte("workflow w:\n  entry: b\n  b -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := "dsl: 2\nimport \"lib/nodes.bot\"\n\nworkflow w:\n  entry: b\n  b -> done\n"
	name := filepath.Join(t.TempDir(), "a1b2c3-main.bot")
	u := LoadDirWithMain(filepath.Join(root, "main.bot"), name, []byte(doc))
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	if string(u.Files[0].Source) != doc || u.Files[0].Name != name || u.Main != "main.bot" || u.Root != root {
		t.Fatalf("main: source %q name %q main %q root %q", u.Files[0].Source, u.Files[0].Name, u.Main, u.Root)
	}
	var rels, names []string
	for _, f := range u.Files[1:] {
		rels = append(rels, f.Rel)
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(rels, []string{"lib/nodes.bot", "lib/schemas.bot"}) || names[0] != filepath.Join(root, "lib", "nodes.bot") {
		t.Fatalf("fragments %v named %v", rels, names)
	}
	if len(u.Merged.Agents) != 1 || u.Merged.Agents[0].Name != "b" || len(u.Merged.Workflows) != 1 {
		t.Fatalf("merged: %d agents, %d workflows", len(u.Merged.Agents), len(u.Merged.Workflows))
	}
	// The identity is the document's, not the disk main's.
	if LoadDir(filepath.Join(root, "main.bot")).Digest == u.Digest {
		t.Fatal("the unit with the document as main has the digest of the unit on disk")
	}
}
