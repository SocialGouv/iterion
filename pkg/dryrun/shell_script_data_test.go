package dryrun

import (
	"context"
	"strings"
	"testing"
)

// #3291: the dry run renders a sh script the way the run does — each data
// reference written for the quoting the shell reads it in — and names the
// run's refusal of a reference standing where no quoting holds a value, as
// a failing finding at that node.
const shellScriptDataBot = `tool quoted:
  language: sh
  script: |
    printf '%s' {{vars.note}} "[{{vars.note}}]" {{vars.langs}}

tool counted:
  language: bash
  script: "echo $(( {{vars.n}} + 1 ))"

workflow shelldata:
  worktree: none
  sandbox: none
  vars:
    note: string = "a'b"
    langs: json = "[1, 2]"
    n: string = "1"
  entry: quoted
  quoted -> counted
  counted -> done
`

func TestADryRunRendersAShellScriptsDataAsTheRunDoes(t *testing.T) {
	t.Parallel()
	rec := &bodyRecorder{}
	r, err := Run(context.Background(), compileBot(t, shellScriptDataBot), Options{Shell: rec})
	if err != nil {
		t.Fatal(err)
	}
	// The declared `json` var is one token, as in a command: the preview
	// reads the workflow's declarations as the run does.
	want := `printf '%s' 'a'\''b' "["'a'\''b'"]" '[1,2]'` + "\n"
	rendered := false
	for _, body := range rec.bodies() {
		rendered = rendered || strings.HasPrefix(body, want)
	}
	if !rendered {
		t.Errorf("the preview rendered %q, want the run's rendering %q", rec.bodies(), want)
	}
	var refused []Finding
	for _, f := range findingsOf(r, KindShellSyntax) {
		if f.Node == "counted" && f.Where == "script" && strings.Contains(f.Detail, "the run refuses this script") && strings.Contains(f.Detail, "{{vars.n}} stands inside arithmetic") {
			refused = append(refused, f)
		}
	}
	if len(refused) == 0 {
		t.Fatalf("the run's refusal of {{vars.n}} in arithmetic was not named: %+v", r.Findings)
	}
	if !r.Failing() {
		t.Fatal("a script the run refuses reads as not failing")
	}
}
