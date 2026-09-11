package runview

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The detached subprocess re-resolves every knob from the workflow, so a
// knob not passed here is silently replaced by the workflow's own value.
// For the permission gate — the anti-prompt-injection boundary — that
// value is usually `off`, which makes a dropped override a fail-OPEN the
// studio still draws as applied.
func TestBuildRunnerCmdCarriesPermissionOnRunAndResume(t *testing.T) {
	for _, command := range []runnerCommand{runnerCommandRun, runnerCommandResume} {
		cmd, err := buildRunnerCmd(context.Background(), "/bin/true", detachedSpec{
			Command: command, RunID: "run-1", FilePath: "wf.bot",
			Permission: "deny",
		})
		if err != nil {
			t.Fatalf("buildRunnerCmd(%s): %v", command, err)
		}
		if joined := strings.Join(cmd.Args, " "); !strings.Contains(joined, "--permission deny") {
			t.Fatalf("%s args = %q, want --permission deny", command, joined)
		}
	}
}

// An empty override must stay absent so the workflow/ITERION_PERMISSION
// precedence chain still decides — the same contract every other
// forwarded knob keeps.
func TestBuildRunnerCmdOmitsEmptyPermission(t *testing.T) {
	for _, command := range []runnerCommand{runnerCommandRun, runnerCommandResume} {
		cmd, err := buildRunnerCmd(context.Background(), "/bin/true", detachedSpec{
			Command: command, RunID: "run-1", FilePath: "wf.bot",
		})
		if err != nil {
			t.Fatalf("buildRunnerCmd(%s): %v", command, err)
		}
		if joined := strings.Join(cmd.Args, " "); strings.Contains(joined, "--permission") {
			t.Fatalf("%s args = %q, want no --permission", command, joined)
		}
	}
}

// The defect Permission had was structural, not a typo: the knob simply did
// not exist on detachedSpec, so nothing pointed at the gap. This sweep
// closes the class — every string-valued field of detachedSpec must reach
// the subprocess argv, or the subprocess re-resolves that knob from the
// workflow and the operator's choice is dropped in silence.
func TestBuildRunnerCmdForwardsEveryDetachedSpecKnob(t *testing.T) {
	// Fields that legitimately do not reach argv as a value, with why.
	launchOnly := map[string]string{
		"MergeInto":  "worktree finalization is a launch-time decision",
		"BranchName": "worktree finalization is a launch-time decision",
	}
	notAValue := map[string]string{
		"Command": "selects the subcommand, it is not passed as a value",
	}

	for _, command := range []runnerCommand{runnerCommandRun, runnerCommandResume} {
		specType := reflect.TypeOf(detachedSpec{})
		for i := 0; i < specType.NumField(); i++ {
			field := specType.Field(i)
			if field.Type.Kind() != reflect.String {
				continue
			}
			if _, skip := notAValue[field.Name]; skip {
				continue
			}
			if _, skip := launchOnly[field.Name]; skip && command == runnerCommandResume {
				continue
			}
			t.Run(string(command)+"/"+field.Name, func(t *testing.T) {
				spec := detachedSpec{Command: command}
				value := reflect.ValueOf(&spec).Elem()
				// A sentinel per field so a forwarded-but-wrong-field bug
				// cannot pass by accident.
				sentinel := "sentinel-" + strings.ToLower(field.Name)
				value.FieldByName(field.Name).SetString(sentinel)
				cmd, err := buildRunnerCmd(context.Background(), "/bin/true", spec)
				if err != nil {
					t.Fatalf("buildRunnerCmd: %v", err)
				}
				if joined := strings.Join(cmd.Args, " "); !strings.Contains(joined, sentinel) {
					t.Fatalf("%s is never forwarded to the runner subprocess (args = %q) — the subprocess will re-resolve it from the workflow", field.Name, joined)
				}
			})
		}
	}
}
