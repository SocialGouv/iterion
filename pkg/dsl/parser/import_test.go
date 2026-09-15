package parser

import (
	"reflect"
	"strings"
	"testing"
)

// `import "lib/x.bot"` lines sit at the head of a file — after the header
// and the leading comments, before any declaration — and are read in every
// profile; the same path twice is one import.
func TestImportsAreReadAtTheHeadOfTheFile(t *testing.T) {
	body := "import \"lib/schemas.bot\"\nimport \"lib/nodes.bot\" ## the phases\nimport \"lib/schemas.bot\"\n\nagent a:\n  description: \"d\"\n"
	for name, head := range map[string]string{
		"profile 1":         "",
		"profile 2":         "dsl: 2\n",
		"after comments":    "## a bot\n\n# note\n",
		"after frontmatter": "## ---\n## name: x\n## ---\ndsl: 2\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot", head+body)
			if len(res.Diagnostics) != 0 {
				t.Fatalf("diagnostics: %v", res.Diagnostics)
			}
			var paths []string
			for _, im := range res.File.Imports {
				paths = append(paths, im.Path)
			}
			if !reflect.DeepEqual(paths, []string{"lib/schemas.bot", "lib/nodes.bot"}) {
				t.Fatalf("imports %v", paths)
			}
			if len(res.File.Agents) != 1 {
				t.Fatalf("the declaration after the imports was lost: %+v", res.File.Agents)
			}
			if res.File.Imports[1].Span.Start.Line == 0 {
				t.Fatalf("an import carries no position")
			}
		})
	}
}

// An import below the first declaration is refused where it is (E044) and
// not recorded: the head is the one place the loader reads.
func TestAnImportAfterADeclarationIsRefused(t *testing.T) {
	for _, src := range []string{
		"agent a:\n  description: \"d\"\n\nimport \"lib/x.bot\"\n",
		"vars:\n  x: string\nimport \"lib/x.bot\"\n",
		"dsl: 2\nprompt p:\n  Text.\n\nimport \"lib/x.bot\"\n",
	} {
		res := Parse("x.bot", src)
		if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagMisplacedImport {
			t.Fatalf("%q: diagnostics %v", src, res.Diagnostics)
		}
		if len(res.File.Imports) != 0 {
			t.Fatalf("%q: a misplaced import was recorded", src)
		}
	}
}

// The path is held to the rules that need no disk: quoted, relative, `/`
// separated, no `..`, a `.bot`, alone on its line.
func TestAnImportPathIsHeldToItsRules(t *testing.T) {
	cases := map[string]string{
		"bare word":     "import lib/x.bot\n",
		"no extension":  "import \"lib/x\"\n",
		"absolute":      "import \"/srv/x.bot\"\n",
		"drive":         "import \"C:/x/x.bot\"\n",
		"backslash":     "import \"lib\\\\x.bot\"\n",
		"parent":        "import \"../x.bot\"\n",
		"parent inside": "import \"lib/../../x.bot\"\n",
		"empty":         "import \"\"\n",
		"empty segment": "import \"lib//x.bot\"\n",
		"junk after":    "import \"lib/x.bot\" extra\n",
		"two on a line": "import \"lib/x.bot\" \"lib/y.bot\"\n",
		"nothing":       "import\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot", "dsl: 2\n"+src+"agent a:\n  description: \"d\"\n")
			var codes []DiagCode
			for _, d := range res.Diagnostics {
				codes = append(codes, d.Code)
			}
			if len(codes) != 1 || codes[0] != DiagBadImportPath {
				t.Fatalf("%q: diagnostics %v", src, res.Diagnostics)
			}
			if len(res.File.Imports) != 0 {
				t.Fatalf("%q: a refused import was recorded", src)
			}
			// The declaration after the refused line is still read.
			if len(res.File.Agents) != 1 {
				t.Fatalf("%q: the file was not read past the refusal", src)
			}
		})
	}
	// The rules are also a function the loader reuses, with a reason.
	if why := ImportPathError("lib/x.bot"); why != "" {
		t.Fatalf("a sound path refused: %s", why)
	}
	if why := ImportPathError("x/../y.bot"); !strings.Contains(why, "..") {
		t.Fatalf("`..` not named: %q", why)
	}
}

// `import` is a keyword like `dsl`: a node, a schema field or a property may
// still be named so, since names are read through tokenAsIdent.
func TestImportIsAKeywordUsableAsAName(t *testing.T) {
	res := Parse("x.bot", "schema s:\n  import: string\n\nagent import:\n  description: \"d\"\n  input: s\n\nworkflow w:\n  entry: import\n  import -> done\n")
	if len(res.Diagnostics) != 0 {
		t.Fatalf("diagnostics: %v", res.Diagnostics)
	}
	if res.File.Agents[0].Name != "import" || res.File.Schemas[0].Fields[0].Name != "import" {
		t.Fatalf("names: %+v %+v", res.File.Agents[0], res.File.Schemas[0])
	}
}

// The header precedes the imports: the lexer takes the profile off the
// file's first significant line, so a `dsl:` below an import was not
// applied — the file was read as profile 1 — and the parser says so (E041)
// rather than record a profile the strings above it never got.
func TestTheHeaderPrecedesTheImports(t *testing.T) {
	res := Parse("x.bot", "import \"lib/x.bot\"\ndsl: 2\n\nagent a:\n  description: \"d\"\n")
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagMisplacedHeader || !strings.Contains(res.Diagnostics[0].Message, "import") {
		t.Fatalf("diagnostics %v", res.Diagnostics)
	}
	if res.File.Profile != 0 || res.File.EffectiveProfile() != 1 {
		t.Fatalf("the AST claims profile %d while the file was read as profile 1", res.File.Profile)
	}
	if len(res.File.Imports) != 1 {
		t.Fatalf("imports %v", res.File.Imports)
	}
}

// The rule keys on the import KEYWORD: a malformed import, recorded
// nowhere, still put a header below it out of the lexer's reach — and a
// header twice, an import between, is named as a duplicate.
func TestTheHeaderPrecedesEvenAMalformedImport(t *testing.T) {
	res := Parse("x.bot", "import \"../x.bot\"\ndsl: 2\n\ntool t:\n  command: \"a\\nb\"\n")
	codes := map[DiagCode]bool{}
	for _, d := range res.Diagnostics {
		codes[d.Code] = true
	}
	if !codes[DiagBadImportPath] || !codes[DiagMisplacedHeader] || len(res.Diagnostics) != 2 {
		t.Fatalf("diagnostics %v", res.Diagnostics)
	}
	if res.File.Profile != 0 || res.File.Tools[0].Command != "a\\nb" {
		t.Fatalf("the AST claims profile %d for a file read as profile 1 (command %q)", res.File.Profile, res.File.Tools[0].Command)
	}
	res = Parse("x.bot", "dsl: 2\nimport \"lib/x.bot\"\ndsl: 2\n\nagent a:\n  description: \"d\"\n")
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagMisplacedHeader || !strings.Contains(res.Diagnostics[0].Message, "duplicate") || res.File.Profile != 2 {
		t.Fatalf("a duplicate header below an import: %v (profile %d)", res.Diagnostics, res.File.Profile)
	}
}
