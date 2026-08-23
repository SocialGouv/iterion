package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func gatedOverrideWorkflow(workflowPermission, nodePermission string) *ir.Workflow {
	agent := &ir.AgentNode{}
	agent.ID = "work"
	agent.Permission = nodePermission
	return &ir.Workflow{
		Name:       "gated",
		Permission: workflowPermission,
		Nodes:      map[string]ir.Node{"work": agent},
	}
}

func backendOverride(selector, backend string) model.ModelOverrides {
	var overrides model.ModelOverrides
	overrides.SetBackend(selector, backend)
	return overrides
}

func TestValidateModelOverridePermissionsRejectsUngatedBackends(t *testing.T) {
	wf := gatedOverrideWorkflow("deny", "")
	for _, backend := range []string{"codex"} {
		t.Run(backend, func(t *testing.T) {
			err := ValidateModelOverridePermissions(wf, backendOverride("agent", backend), "")
			if err == nil {
				t.Fatalf("permission: deny override to %s was admitted", backend)
			}
			if !strings.Contains(err.Error(), backend) || !strings.Contains(err.Error(), "UNGATED") {
				t.Errorf("error = %q, want backend and UNGATED", err)
			}
		})
	}
}

// grok and kimi enforce deny: (#498) but cannot PAUSE for ask: rules — an
// ask-gated workflow retargeted to them would drop its declared gate.
func TestValidateModelOverridePermissionsRejectsAskOnDenyOnlyBackends(t *testing.T) {
	wf := gatedOverrideWorkflow("ask", "")
	wf.PermissionAsk = []string{"Bash(rm:*)"}
	for _, backend := range []string{"grok", "kimi"} {
		t.Run(backend, func(t *testing.T) {
			err := ValidateModelOverridePermissions(wf, backendOverride("agent", backend), "")
			if err == nil || !strings.Contains(err.Error(), backend) {
				t.Fatalf("permission: ask override to %s: got %v, want a refusal naming the backend", backend, err)
			}
		})
	}
}

func TestValidateModelOverridePermissionsKeepsGateEnforcingBackends(t *testing.T) {
	wf := gatedOverrideWorkflow("deny", "")
	for _, backend := range []string{"claw", "claude_code", "pi", "grok", "kimi"} {
		t.Run(backend, func(t *testing.T) {
			if err := ValidateModelOverridePermissions(wf, backendOverride("*", backend), ""); err != nil {
				t.Fatalf("gate-enforcing backend %s rejected: %v", backend, err)
			}
		})
	}
}

func TestValidateModelOverridePermissionsUsesResolvedPrecedence(t *testing.T) {
	wf := gatedOverrideWorkflow("deny", "ask")
	unsafe := backendOverride("*", "codex")
	if err := ValidateModelOverridePermissions(wf, unsafe, "off"); err != nil {
		t.Fatalf("run-level permission: off must win: %v", err)
	}
	if err := ValidateModelOverridePermissions(wf, unsafe, ""); err == nil || !strings.Contains(err.Error(), "ask") {
		t.Fatalf("node permission: ask must win over workflow deny, got %v", err)
	}
}

func TestValidateModelOverridePermissionsUsesTheResolvedSelector(t *testing.T) {
	wf := gatedOverrideWorkflow("deny", "")
	var overrides model.ModelOverrides
	overrides.SetBackend("*", "codex")
	// The exact rule is more specific and therefore the backend that will
	// actually run. Screening raw rows would reject this safe composition.
	overrides.SetBackend("work", "claw")
	if err := ValidateModelOverridePermissions(wf, overrides, ""); err != nil {
		t.Fatalf("resolved safe override rejected: %v", err)
	}
}

func TestLaunchCloudRejectsUngatedOverrideBeforePublish(t *testing.T) {
	const source = `
agent work:
  backend: "claude_code"
  model: "claude-opus-5"
  system: p
  tools: [read_file]

prompt p:
  Do the work.

workflow gated:
  entry: work
  permission: deny
  work -> done
`
	pub := &stubLaunchPublisher{}
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Launch(context.Background(), LaunchSpec{
		FilePath: "gated.bot",
		Source:   source,
		ModelOverrides: []ModelOverrideEntry{
			{Selector: "agent", Backend: "codex"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "UNGATED") {
		t.Fatalf("cloud launch error = %v, want synchronous UNGATED refusal", err)
	}
	if pub.lastSpec != nil {
		t.Fatal("unsafe launch reached the publisher")
	}
}

// The cloud screen resolves a mode the runner never receives (#493), so it is
// deliberately fail-closed in BOTH directions. These two launches are the
// divergent pair: each is admitted by exactly one of the resolutions, and
// neither may reach the publisher.
func TestLaunchCloudScreensBothPermissionResolutions(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		permission string
	}{
		{
			// Workflow gates, operator's run-level "off" does not reach the
			// pod: the pod would refuse after the queue slot is spent.
			name: "run level off cannot unlock a gated workflow",
			source: `
agent work:
  backend: "claude_code"
  model: "claude-opus-5"
  system: p
  tools: [read_file]

prompt p:
  Do the work.

workflow gated:
  entry: work
  permission: deny
  work -> done
`,
			permission: "off",
		},
		{
			// Workflow is ungated, the operator selected a gate: the pod
			// resolves "off" and would run the override with no gate at all.
			name: "run level deny cannot be honoured by the pod",
			source: `
agent work:
  backend: "claude_code"
  model: "claude-opus-5"
  system: p
  tools: [read_file]

prompt p:
  Do the work.

workflow ungated:
  entry: work
  work -> done
`,
			permission: "deny",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &stubLaunchPublisher{}
			svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			_, err = svc.Launch(context.Background(), LaunchSpec{
				FilePath:   "wf.bot",
				Source:     tc.source,
				Permission: tc.permission,
				ModelOverrides: []ModelOverrideEntry{
					{Selector: "agent", Backend: "codex"},
				},
			})
			if err == nil || !strings.Contains(err.Error(), "UNGATED") {
				t.Fatalf("cloud launch error = %v, want synchronous UNGATED refusal", err)
			}
			if pub.lastSpec != nil {
				t.Fatal("unsafe launch reached the publisher")
			}
		})
	}
}
