package bots

import (
	"bytes"
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// parseBotUnit parses a catalog bot as its UNIT — the file and the fragments
// its imports reach (`import "lib/x.bot"`) — and returns what parser.Parse
// returns for a single file, so a test that read main.bot alone reads the
// whole program. A bot in one file parses exactly as before.
func parseBotUnit(path string) *parser.ParseResult {
	u := unit.LoadDir(path)
	return &parser.ParseResult{File: u.Merged, Diagnostics: u.Diagnostics}
}

// botUnitSource is the text of a catalog bot's unit — main first, then the
// fragments its imports reach — for the tests that read the DSL as text: a
// clause in a prompt, a `skills:` list, a tool's command body. A file that
// is not a unit (a bot in one file, a skill, a manifest) reads byte for
// byte as os.ReadFile does.
func botUnitSource(path string) ([]byte, error) {
	u := unit.LoadDir(path)
	if len(u.Files) <= 1 {
		return os.ReadFile(path)
	}
	var b bytes.Buffer
	for _, f := range u.Files {
		b.Write(f.Source)
		if !bytes.HasSuffix(f.Source, []byte("\n")) {
			b.WriteByte('\n')
		}
	}
	return b.Bytes(), nil
}
