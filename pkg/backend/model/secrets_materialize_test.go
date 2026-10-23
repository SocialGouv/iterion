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

// Exercise the launcher-built guard and the real shell recipe, including its
// command rewrite and env materialization seams. Undeclared literals stay in
// the command itself; checking restored stdout alone misses substitutions.
func TestToolNodeKeepsUndeclaredIterionLiterals(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"TOKEN": {Value: "declared-value-12345"},
	}}
	guard := BuildSecretGuard(context.Background(), wf, nil, nil)
	for _, tc := range []struct{ name, command, want string }{
		{"W1 pipeline", "printf '%s' ITERION_A | grep -c ITERION_A", "1"},
		{"W2 identical arguments", "test ITERION_A = ITERION_A && printf '%s' --set=k=ITERION_A", "--set=k=ITERION_A"},
		{"W3 declared", "printf '%s' __ITERION_SECRET_TOKEN__", "declared-value-12345"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged string
			e := newTestClawExecutor(NewRegistry(), wf, WithSecretGuard(guard), WithWorkDir(t.TempDir()),
				WithEventHooks(EventHooks{OnToolNodeResult: func(_, _ string, input []byte, _ string, _ time.Duration, _ error) {
					logged = string(input)
				}}))
			// The delegated backend receives this exact launcher-side closure.
			materialize := e.secretMaterializer()
			if materialize == nil {
				t.Fatal("declared secret does not install a materializer")
			}
			if tc.name != "W3 declared" && materialize(tc.command) != tc.command {
				t.Fatal("launcher materializer changes an undeclared literal")
			}
			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "literal"}, Command: tc.command}
			out, err := e.Execute(context.Background(), node, nil)
			if err != nil {
				t.Fatal(err)
			}
			if logged != tc.command || out["result"] != tc.want {
				t.Fatalf("command = %q, output = %#v; want %q / %q", logged, out, tc.command, tc.want)
			}
		})
	}
}
