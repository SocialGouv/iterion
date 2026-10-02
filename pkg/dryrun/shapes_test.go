package dryrun

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// capturingShell records every body the dry run hands its checker, which is
// the body the dry run REPORTS.
type capturingShell struct {
	mu   sync.Mutex
	seen []string
}

func (c *capturingShell) Check(interpreter, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, text)
	return nil
}

func (c *capturingShell) contains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.seen {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

const shapesBot = `vars:
  langs: json = "[1, 2]"
  files: string[] = "a.go,b.go"

tool render:
  command: "LANGS={{vars.langs}} run {{vars.files}}"

workflow w:
  entry: render
  render -> done
`

// A dry run must render what the run renders. It reads the same
// declarations through the same helper, so a `json` var is one token here
// too and a `string[]` is argv words — otherwise the report shows a
// command line the run never issues.
func TestDryRun_RendersByTheWorkflowsDeclarations(t *testing.T) {
	shell := &capturingShell{}
	if _, err := Run(context.Background(), compileBot(t, shapesBot), Options{Shell: shell}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(shell.seen) == 0 {
		t.Fatal("the dry run checked no shell body")
	}
	if want := `LANGS='[1,2]'`; !shell.contains(want) {
		t.Errorf("dry run rendered %q, want it to contain %q", shell.seen, want)
	}
	if want := `run 'a.go' 'b.go'`; !shell.contains(want) {
		t.Errorf("dry run rendered %q, want it to contain %q", shell.seen, want)
	}
}
