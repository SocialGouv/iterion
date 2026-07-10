package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestToolNodeShellMaterializesSecret proves Layer 1: a `{{secrets.X}}`
// reference in a tool node command runs with the REAL value, but the
// command text handed to the hooks/logs only ever carries the
// placeholder.
func TestToolNodeShellMaterializesSecret(t *testing.T) {
	const realVal = "sk-REAL-abcdef0123456789ABCDEF"
	const ph = "__ITERION_SECRET_mytoken__"

	guard := secretguard.New([]secretguard.Secret{
		{Name: "mytoken", Value: realVal, Placeholder: ph},
	}, secretguard.DefaultConfig())

	var loggedInput string
	var loggedOutput string
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
		WithSecretGuard(guard),
		WithEventHooks(EventHooks{
			OnToolNodeResult: func(_, _ string, input []byte, output string, _ time.Duration, _ error) {
				loggedInput = string(input)
				loggedOutput = output
			},
		}),
	)

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "fetch"},
		Command:  "echo {{secrets.mytoken}}",
		CommandRefs: []*ir.Ref{
			{Kind: ir.RefSecrets, Path: []string{"mytoken"}, Raw: "{{secrets.mytoken}}"},
		},
	}

	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The command actually ran with the real value (echo printed it).
	if got, _ := out["result"].(string); !strings.Contains(got, realVal) {
		t.Errorf("tool output should contain the real value (materialised); got %q", got)
	}
	if !strings.Contains(loggedOutput, realVal) {
		t.Errorf("stdout should contain the real value; got %q", loggedOutput)
	}

	// The logged command text carries ONLY the placeholder.
	if strings.Contains(loggedInput, realVal) {
		t.Errorf("logged command must not contain the real value: %q", loggedInput)
	}
	if !strings.Contains(loggedInput, ph) {
		t.Errorf("logged command should carry the placeholder: %q", loggedInput)
	}
}
