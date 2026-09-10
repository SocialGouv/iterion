package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestWorkspaceCheckpointEnabled pins the precedence chain, including the two
// answers that decide whether an audit publishes its findings: a workflow
// that says off must be obeyed even when the deployment's env says on, and an
// unreadable value at any layer must fall THROUGH rather than read as off (a
// net silently not laid is the failure this switch must not introduce).
func TestWorkspaceCheckpointEnabled(t *testing.T) {
	cases := []struct {
		name string
		wf   string // workflow-block value ("" = field unset)
		env  string // ITERION_WORKSPACE_CHECKPOINT ("" = unset)
		want bool
	}{
		{"default is on", "", "", true},
		{"workflow off", "off", "", false},
		{"workflow on", "on", "", true},
		{"env off", "", "off", false},
		{"env on", "", "on", true},
		{"env accepts 0", "", "0", false},
		{"workflow beats env", "off", "on", false},
		{"workflow beats env, other way", "on", "off", true},
		{"unreadable workflow value falls through to the default", "bogus", "", true},
		{"unreadable env value falls through to the default", "", "bogus", true},
		{"unreadable workflow value falls through to the env", "bogus", "off", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_WORKSPACE_CHECKPOINT", tc.env)
			wf := &ir.Workflow{WorkspaceCheckpoint: tc.wf}
			if got := WorkspaceCheckpointEnabled(wf); got != tc.want {
				t.Fatalf("WorkspaceCheckpointEnabled(wf=%q, env=%q) = %v, want %v", tc.wf, tc.env, got, tc.want)
			}
		})
	}
}

// TestWorkspaceCheckpointEnabledNilWorkflow: the resolver is called from the
// runner's launch path, where a workflow is always present — but a nil must
// not panic, and must not silently withdraw a net either.
func TestWorkspaceCheckpointEnabledNilWorkflow(t *testing.T) {
	t.Setenv("ITERION_WORKSPACE_CHECKPOINT", "")
	if !WorkspaceCheckpointEnabled(nil) {
		t.Fatal("a nil workflow must inherit the default (on), not withdraw the net")
	}
}
