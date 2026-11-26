// The helpers below are copies of bots' own; bots keeps its copy for the
// behaviour tests that stay there.

package catalogcorpus

import (
	"bytes"
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// parseBotUnit parses a catalog bot as its UNIT — the file and the fragments
// its imports reach (`import "lib/x.bot"`) — and returns what parser.Parse
// returns for a single file, so a test that read main.bot alone reads the
// whole program. A bot in one file parses exactly as before.
func parseBotUnit(path string) *parser.ParseResult {
	u := unittest.Whole(unit.LoadDir(path))
	return &parser.ParseResult{File: u.Merged, Diagnostics: u.Diagnostics}
}

// botUnitSource is the text of a catalog bot's unit — main first, then the
// fragments its imports reach — for the tests that read the DSL as text: a
// clause in a prompt, a `skills:` list, a tool's command body. A file that
// is not a unit (a bot in one file, a skill, a manifest) reads byte for
// byte as os.ReadFile does.
func botUnitSource(path string) ([]byte, error) {
	u := unittest.Whole(unit.LoadDir(path))
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

// itoa avoids pulling strconv in for one call site in a fixture generator.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}

// planPhaseBots are the campaign bots carrying the cross-model plan
// phase (author → peer critique → author revise → campaign). The list is
// the contract: adding the phase to another bot means adding it here so
// the wiring below is guarded for it too.
var planPhaseBots = []string{
	"feature-dev",
	"app-dev",
	"branch-improve-loop",
	"whole-improve-loop",
	"feature-gap-fill",
	"test-coverage",
	"e2e-coverage",
}
