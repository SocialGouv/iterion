package delegate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// W1/W2 redden if Materialize rewrites undeclared ITERION literals, including
// per-occurrence tokens restored only after execution. W3 reddens if declared
// placeholders stop materializing or the output stops returning placeholders.
func TestToolRewritePreservesUndeclaredLiterals(t *testing.T) {
	const value = "declared-value-12345"
	g := secretguard.New([]secretguard.Secret{
		{Name: "TOKEN", Value: value},
		{Name: "CUSTOM", Placeholder: "ITERION_DECLARED", Value: value},
	}, secretguard.DefaultConfig())
	bin := filepath.Join(t.TempDir(), "rewriter")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'env %s' \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	chain := rewrite.NewChain([]plugin.RewriterSpec{{ID: "test", Locate: plugin.LocateSpec{Paths: []string{bin}},
		Invoke: plugin.InvokeSpec{Argv: []string{"{{command}}"}, ApplyExitCodes: []int{0}}}})
	compress := rewriteCommandHandler(chain, rewrite.On, g.Materialize)
	materialize := materializeSecretsHandler(g.Materialize)
	unmaterialize := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, g.NamesMaterializableSecret, t.TempDir())
	for _, tc := range []struct {
		name, command, wantCommand, wantOutput string
		template                               bool
	}{
		{"W1 pipeline", "printf '%s' ITERION_A | grep -c ITERION_A", "env printf '%s' ITERION_A | grep -c ITERION_A", "1\n", false},
		{"W2 helm template", `printf '%s' '{{ eq "ITERION_A" "ITERION_A" }}:{{ len "ITERION_A" }}'`, `env printf '%s' '{{ eq "ITERION_A" "ITERION_A" }}:{{ len "ITERION_A" }}'`, "true:9", true},
		{"helm set argument", "printf '%s ' --set k=ITERION_A", "env printf '%s ' --set k=ITERION_A", "--set k=ITERION_A ", false},
		{"undeclared placeholder", "printf '%s' __ITERION_SECRET_UNKNOWN__", "env printf '%s' __ITERION_SECRET_UNKNOWN__", "__ITERION_SECRET_UNKNOWN__", false},
		{"W3 declared placeholder", "printf '%s' __ITERION_SECRET_TOKEN__", "printf '%s' " + value, value, false},
		{"W3 declared custom literal", "printf '%s' ITERION_DECLARED", "printf '%s' " + value, value, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"command": tc.command}
			call := claudesdk.HookCallbackInput{ToolName: "Bash", ToolInput: input}
			command := tc.command
			// Both PreToolUse hooks receive the original input; the SDK keeps
			// the updated input. Secret commands bypass compression entirely.
			for _, hook := range []claudesdk.HookCallback{compress, materialize} {
				out, err := hook(context.Background(), call)
				if err != nil {
					t.Fatal(err)
				}
				if out.UpdatedInput != nil {
					command = out.UpdatedInput["command"].(string)
				}
			}
			if command != tc.wantCommand {
				t.Fatalf("rewritten command = %q, want %q", command, tc.wantCommand)
			}
			cmd := exec.Command("bash", "-c", command)
			cmd.Dir = t.TempDir()
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("tool command: %v, output %q", err, raw)
			}
			if tc.template {
				tmpl, err := template.New("helm").Parse(string(raw))
				if err != nil {
					t.Fatal(err)
				}
				var rendered bytes.Buffer
				if err := tmpl.Execute(&rendered, nil); err != nil {
					t.Fatal(err)
				}
				raw = rendered.Bytes()
			}
			if string(raw) != tc.wantOutput {
				t.Fatalf("tool output = %q, want %q", raw, tc.wantOutput)
			}
			call.ToolResponse = map[string]any{"stdout": string(raw)}
			restored, err := unmaterialize(context.Background(), call)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(tc.name, "W3") {
				out, _ := restored.UpdatedToolOutput.(map[string]any)
				if out["stdout"] != secretguard.PlaceholderForName("TOKEN") {
					t.Fatalf("declared value does not return to its placeholder: %+v", restored)
				}
			} else if restored.UpdatedToolOutput != nil {
				t.Fatalf("undeclared output changes: %+v", restored)
			}
		})
	}
}
