package botscaffold

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestGoalLoopCheckIsTheVerdict runs the goal-loop shape's RENDERED command,
// the way the runtime does, because the whole shape rests on one claim: the
// verdict is the condition's exit code, not something a model can be talked
// into. Three conditions, three verdicts, and the one that matters most is
// the EMPTY one — `bash -c ''` exits 0, so a shape that passed it through
// would report a reached goal nobody checked, on the first pass, forever.
func TestGoalLoopCheckIsTheVerdict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shape's command is a bash command")
	}
	// Named, and a failure under CI: a runner missing one is a runner to
	// fix, and the message sends the reader there, not to the template.
	requireBins(t, "bash", "jq")
	tpl, ok := TemplateByID("goal-loop")
	if !ok {
		t.Fatal("the goal-loop template is gone")
	}
	spec := tpl.Spec
	spec.Slug = "exec-goal-loop"
	_, w, _ := scaffoldAndCompile(t, spec)
	tools := nodesOf[*ir.ToolNode](w)
	if len(tools) != 1 {
		t.Fatalf("want the one check tool, got %d", len(tools))
	}

	for _, c := range []struct {
		name      string
		condition string
		wantMet   bool
		// refuses says the node's own command must FAIL, naming why,
		// instead of returning a verdict.
		refuses  bool
		wantSaid string
	}{
		{name: "met", condition: "echo 'all green'; true", wantMet: true},
		{name: "not met", condition: "echo 'two checks still red' >&2; false", wantMet: false},
		{
			name: "empty condition is refused, never treated as met",
			// What an unset `goal_condition` renders to. `bash -c ''`
			// exits 0, so passing it through would be a goal reached
			// before anything ran.
			condition: "",
			refuses:   true,
			wantSaid:  "the stop condition is empty",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			command := strings.ReplaceAll(tools[0].Command, "{{vars.goal_condition}}", singleQuote(c.condition))
			if strings.Contains(command, "{{") {
				t.Fatalf("a ref went unsubstituted, the test no longer runs what the runtime does: %q", command)
			}
			var stdout, stderr bytes.Buffer
			// bash, the shell the runtime pins for a tool node's command
			// (executor_tool.go): /bin/sh is dash on Debian-derived images
			// and cannot read the bashisms the command relies on.
			sh := exec.Command("bash", "-c", command)
			sh.Stdout, sh.Stderr = &stdout, &stderr
			err := sh.Run()

			if c.refuses {
				if err == nil {
					t.Fatalf("the command returned a verdict for an empty condition: %q", stdout.String())
				}
				if !strings.Contains(stderr.String(), c.wantSaid) {
					t.Fatalf("the refusal does not say why (stderr %q), so an operator reads a failed node and not an unset var", stderr.String())
				}
				if strings.Contains(stdout.String(), "\"met\"") {
					t.Fatalf("the refusal still printed a verdict on stdout: %q", stdout.String())
				}
				return
			}

			if err != nil {
				t.Fatalf("the node's own command must succeed whatever the condition says: %v (stderr %q)", err, stderr.String())
			}
			var out map[string]any
			if jerr := json.Unmarshal(stdout.Bytes(), &out); jerr != nil {
				t.Fatalf("stdout is not the single JSON object the runtime parses (%v); the condition's output leaked into it: %q", jerr, stdout.String())
			}
			met, isBool := out["met"].(bool)
			if !isBool {
				t.Fatalf("the node's output carries no bool `met` (%v); the exit edge would read nil and the loop would never stop", out)
			}
			if met != c.wantMet {
				t.Errorf("met = %v, want %v (condition %q)", met, c.wantMet, c.condition)
			}
			// The condition's own log travels in the output for the next
			// pass's prompt to read — carried, never the verdict.
			if _, isString := out["log"].(string); !isString {
				t.Errorf("the node's output carries no string `log` (%v); the next pass would have nothing to read", out)
			}
		})
	}
}
