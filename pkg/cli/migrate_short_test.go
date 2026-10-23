package cli

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/migrate"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// `iterion dsl migrate` writes no manifest floor off a walk cut short (r3a
// F4): migrating the bundle to profile 2, child.bot's fragments are past the
// read budget, one of them uses `tools: []`, and the floor read off the rest
// would leave the manifest below the release that reads the empty list. The
// run is refused before a byte moves — child.bot named with its remedy, every
// file's bytes kept; read whole, the floor is raised.
func TestMigrateWritesNoFloorOffASourceCutShort(t *testing.T) {
	main := "prompt p:\n  Do.\n\nagent a:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: p\n\nworkflow main:\n  entry: a\n  a -> done\n"
	child := "import \"lib/big.bot\"\nimport \"lib/k.bot\"\n\nworkflow c:\n  entry: helper\n  helper -> done\n"
	k := "prompt mission:\n  Do.\n\nagent helper:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  tools: []\n"
	t.Setenv(parser.ReadBudgetEnv, "0")
	pr, err := parser.Parse("child.bot", child)
	if err != nil {
		t.Fatal(err)
	}
	budget := 400
	big := "prompt big:\n" + strings.Repeat("  x\n", budget-pr.Spent-2)
	manifest := "name: floorbot\nversion: 1.0.0\nrequires:\n  iterion: \">= " + parser.ImportSince + "\"\n"
	write := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "floorbot")
		for rel, src := range map[string]string{"main.bot": main, "child.bot": child, "lib/big.bot": big, "lib/k.bot": k, "manifest.yaml": manifest} {
			full := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	t.Run("cut short", func(t *testing.T) {
		dir := write(t)
		t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
		_, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, To: 2, Floor: parser.ImportSince})
		if !errors.Is(err, migrate.ErrRefused) || !strings.Contains(err.Error(), "child.bot") || !strings.Contains(err.Error(), "C256") || !strings.Contains(err.Error(), parser.ReadBudgetEnv) {
			t.Errorf("migrate over a source cut short: %v — want the refusal naming child.bot, C256, the budget's variable", err)
		}
		for rel, src := range map[string]string{"manifest.yaml": manifest, "main.bot": main, "child.bot": child, "lib/k.bot": k} {
			got, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if sha256.Sum256(got) != sha256.Sum256([]byte(src)) {
				t.Errorf("%s was written by a run refused for a walk cut short: %q", rel, got)
			}
		}
	})
	t.Run("read whole", func(t *testing.T) {
		dir := write(t)
		t.Setenv(parser.ReadBudgetEnv, "0")
		if _, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, To: 2, Floor: parser.ImportSince}); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
		if string(got) == manifest {
			t.Errorf("read whole, the manifest's floor is not raised to the release the empty list needs: %q", got)
		}
	})
}
