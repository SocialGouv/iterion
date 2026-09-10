package ir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const relativeIncludeSource = "prompt p:\n  {{include \"rules.md\"}}\n\nagent a:\n  model: \"m\"\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"

// An include resolves beside the prompt's source file, named in full. A
// RELATIVE name that happens to exist in the process working directory —
// a `main.bot` beside a server started from a bot's directory — must not
// resolve against that directory: the name is not the file the document
// came from, it is whatever the process sits in. The same absolute-path
// rule InlinePromptIncludes applies at export applies at compile.
func TestCompilePromptInclude_RefusesARelativeSourceName(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"rules.md": "RULES", "main.bot": relativeIncludeSource} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	pr := parser.Parse("main.bot", relativeIncludeSource) // a name that exists in the cwd
	cr := Compile(pr.File)
	var refused bool
	for _, d := range cr.Diagnostics {
		if d.Code == DiagBadPromptInclude {
			refused = true
			if !strings.Contains(d.Message, "absolute") {
				t.Errorf("the refusal does not say the name is not absolute: %s", d.Message)
			}
		}
	}
	if !refused {
		t.Fatalf("a relative source name resolved its include against the working directory: %v", cr.Diagnostics)
	}

	prAbs := parser.Parse(filepath.Join(dir, "main.bot"), relativeIncludeSource)
	crAbs := Compile(prAbs.File)
	for _, d := range crAbs.Diagnostics {
		if d.Code == DiagBadPromptInclude {
			t.Fatalf("the absolute name was refused: %s", d.Message)
		}
	}
	if crAbs.Workflow == nil || !strings.Contains(crAbs.Workflow.Prompts["p"].Body, "RULES") {
		t.Fatalf("the include beside the absolute source did not resolve: %+v", crAbs.Workflow)
	}
}
