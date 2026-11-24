package dryrun

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// #3291: the dry run renders a sh script the way the run does — each data
// reference a read of the variable the run hands the interpreter, the value
// never in the text — so the text it holds to `sh -n` is the run's.
const shellScriptDataBot = `tool quoted:
  language: sh
  script: |
    printf '%s' {{vars.note}} "[{{vars.note}}]" {{vars.langs}}
    echo $(( {{vars.n}} + 1 ))
    cat <<EOF
    {{vars.note}}
    EOF

workflow shelldata:
  worktree: none
  sandbox: none
  vars:
    note: string = "a'b $(touch pwned)"
    langs: json = "[1, 2]"
    n: string = "1"
  entry: quoted
  quoted -> done
`

func TestADryRunRendersAShellScriptsDataAsTheRunDoes(t *testing.T) {
	t.Parallel()
	rec := &bodyRecorder{}
	if _, err := Run(context.Background(), compileBot(t, shellScriptDataBot), Options{Shell: rec}); err != nil {
		t.Fatal(err)
	}
	read := regexp.MustCompile(`\$\{(__ITERION_DATA_[0-9a-f]{16}_[0-9]+(_JSON)?__)(\+"\$__ITERION_DATA_[0-9a-f]{16}_[0-9]+__")?\}`)
	want := "printf '%s' ${R} \"[${R}]\" ${R}\necho $(( ${R} + 1 ))\ncat <<EOF\n${J}\nEOF\n"
	rendered := false
	for _, body := range rec.bodies() {
		if strings.Contains(body, "a'b") || strings.Contains(body, "touch") || strings.Contains(body, "[1,2]") {
			t.Errorf("a value is in the previewed script's text:\n%s", body)
		}
		shape := read.ReplaceAllStringFunc(body, func(m string) string {
			switch {
			case strings.Contains(m, "+"):
				return "${R}"
			case strings.HasSuffix(m, "_JSON__}"):
				return "${J}"
			}
			return "${V}"
		})
		rendered = rendered || strings.HasPrefix(shape, want)
	}
	if !rendered {
		t.Errorf("the preview rendered %q, want the run's reads %q", rec.bodies(), want)
	}
}
