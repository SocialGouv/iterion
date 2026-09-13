package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// `iterion validate main.bot` run from the bot's own directory: the include
// resolves beside the file, as it always has, although the compiler now
// refuses a relative source name — validate hands it the path in full.
func TestValidate_ResolvesAnIncludeBesideARelativePath(t *testing.T) {
	dir := t.TempDir()
	src := "prompt p:\n  {{include \"rules.md\"}}\n\nagent a:\n  model: \"m\"\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"
	writeFixture(t, dir, "main.bot", src)
	if err := os.WriteFile(filepath.Join(dir, "rules.md"), []byte("RULES"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	p, buf := newTestPrinter(cli.OutputJSON)
	_ = cli.RunValidate("main.bot", p)
	var result cli.ValidateResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "C055") {
		t.Fatalf("the include beside a relative path was refused:\n%s", buf.String())
	}
	if !result.Valid {
		t.Fatalf("validate refused a bot whose include sits beside it:\n%s", buf.String())
	}
}
