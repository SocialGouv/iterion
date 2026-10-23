package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A main the read budget refuses holds its workflow past the cut: the
// verdict's cause is the refusal, never "no workflow found", which the
// empty program the refusal left would say of a file that has one.
func TestAValidateOfABotPastTheBudgetNamesTheRefusal(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "200")
	dir := t.TempDir()
	path := filepath.Join(dir, "main.bot")
	src := "prompt p:\n  Do.\n\nagent a:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n" + strings.Repeat("## a comment line\n", 200)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	var human bytes.Buffer
	if err := RunValidate(path, &Printer{W: &human, Format: OutputHuman}); err == nil {
		t.Fatal("a bot past the read budget validated")
	}
	out := human.String()
	if !strings.Contains(out, "result: INVALID (not read whole: the bot is past the read budget of 200") || strings.Contains(out, "no workflow found") {
		t.Errorf("the verdict does not name the refusal as its cause:\n%s", out)
	}
	var js bytes.Buffer
	_ = RunValidate(path, &Printer{W: &js, Format: OutputJSON})
	var result ValidateResult
	if err := json.Unmarshal(js.Bytes(), &result); err != nil {
		t.Fatalf("json: %v\n%s", err, js.String())
	}
	if result.Valid || len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0].Message, "past the read budget") {
		t.Errorf("json verdict %+v, want invalid with the refusal alone", result)
	}
	for _, d := range result.Diagnostics {
		if strings.Contains(d.Message, "no workflow found") {
			t.Errorf("json carries %q", d.Message)
		}
	}
}

// The same verdict, for every reading that failed: an author document its
// reader refused whole (E050, here past the read budget) was not read, and
// a .bot whose program did not parse has that as its cause — neither is
// "no workflow found", which the empty or partial program would say.
func TestAValidateVerdictNamesTheReadingThatFailed(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "60")
	dir := t.TempDir()
	for name, c := range map[string]struct {
		file, src, want string
	}{
		"a document past the budget": {"hello.bot.yaml", "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n" + strings.Repeat("# a comment line\n", 200), "result: INVALID (not read: the document may hold more values"},
		"a .bot that does not parse": {"broken.bot", "workflow: w\n", "result: INVALID (does not parse: expected workflow name)"},
		"a .bot that is not UTF-8":   {"latin1.bot", "workflow w:\n  entry: a\n\nagent a:\n  backend: \"claude_code\"\n  system: \"caf\xe9\"\n", "result: INVALID (not read: the source is not UTF-8 text"},
	} {
		path := filepath.Join(dir, c.file)
		if err := os.WriteFile(path, []byte(c.src), 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := RunValidate(path, &Printer{W: &out, Format: OutputHuman}); err == nil {
			t.Fatalf("%s validated", name)
		}
		if !strings.Contains(out.String(), c.want) || strings.Contains(out.String(), "no workflow found") {
			t.Errorf("%s: the verdict does not name the failed reading:\n%s", name, out.String())
		}
	}
}
