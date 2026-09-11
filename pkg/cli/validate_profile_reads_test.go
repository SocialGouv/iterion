package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `iterion validate` tells a headerless file that profile 2 would read it
// otherwise (C144), with the counts and the first line — and says nothing
// to a headerless file both profiles read alike, or to a profile-2 file.
func TestRunValidate_SaysWhenProfileOneIsAssumedAndMatters(t *testing.T) {
	inTempWorkspace(t)
	matters := "prompt p:\n  First.\n\n  Second.\n\ntool t:\n  command: \"a\\nb\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	alike := "prompt p:\n  One line.\n\ntool t:\n  command: \"plain\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	for name, src := range map[string]string{"matters.bot": matters, "alike.bot": alike, "two.bot": "dsl: 2\n" + matters} {
		if err := os.WriteFile(filepath.Join(".", name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jp, out := jsonPrinter()
	if err := RunValidate("matters.bot", jp); err != nil {
		t.Fatalf("validate matters: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C144") || !strings.Contains(s, "1 quoted literal(s) hold a backslash, 1 blank line(s)") || !strings.Contains(s, "first at line 3") {
		t.Fatalf("validate did not say profile 1 matters:\n%s", s)
	}
	for _, name := range []string{"alike.bot", "two.bot"} {
		jp, out = jsonPrinter()
		if err := RunValidate(name, jp); err != nil {
			t.Fatalf("validate %s: %v\n%s", name, err, out.String())
		}
		if strings.Contains(out.String(), "C144") {
			t.Fatalf("%s drew C144:\n%s", name, out.String())
		}
	}
}
