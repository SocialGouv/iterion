package botregistry

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The launch form's schema of a bot the read budget cuts short is its
// refusal, with the remedy — never the vars of what the load read, which
// lack the ones its unread fragments declare.
func TestTheSchemaOfABotCutShortIsItsRefusal(t *testing.T) {
	ClearSchemaCache()
	t.Cleanup(ClearSchemaCache)
	dir := t.TempDir()
	main := "import \"lib/big.bot\"\nimport \"lib/v.bot\"\n\nagent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
	big := "prompt big:\n" + strings.Repeat("  x\n", 300)
	writeFile(t, filepath.Join(dir, "bot", "main.bot"), main)
	writeFile(t, filepath.Join(dir, "bot", "lib", "big.bot"), big)
	writeFile(t, filepath.Join(dir, "bot", "lib", "v.bot"), "vars:\n  ticket: string\n")
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(200))
	e := Entry{Name: "bot", Path: filepath.Join(dir, "bot", "main.bot")}
	vars, _, err := LoadSchema(e)
	if err == nil || vars != nil || !strings.Contains(err.Error(), "past the read budget of 200") || !strings.Contains(err.Error(), parser.ReadBudgetEnv) {
		t.Errorf("the schema of a bot cut short: vars %+v, err %v — want its refusal with the remedy, no vars", vars, err)
	}
}
