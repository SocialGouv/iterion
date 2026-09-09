package cloudpublisher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The snapshot's entry keeps the launch bot's own file name — a loose
// catalog .bot is not called main.bot — and the parse is attributed to THAT
// file: it is the file diagnostics name, and the directory its includes are
// looked for beside. Attributing every launch to main.bot named a file that
// may not exist, which InlinePromptIncludes then refused.
func TestSnapshotLaunchIsParsedAsItsOwnEntryFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"main.bot":  "workflow main:\n  entry: done\n",
		"probe.bot": "prompt p:\n  hello\n\nworkflow main:\n  entry: done\n  this line is not a property\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile(filepath.Join(dir, "probe.bot"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = marshalIRFromSpec("bots/loose/probe.bot", string(source), dir)
	if err == nil {
		t.Fatal("a source with a syntax error was accepted")
	}
	want := filepath.Join(dir, "probe.bot")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("the parse is not attributed to the launch bot's own file %s: %v", want, err)
	}
}
