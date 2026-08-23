package permissionhook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

func writePolicy(t *testing.T, mode permission.Mode, allow, ask, deny []string) string {
	t.Helper()
	p, err := permission.NewPolicy(mode, allow, ask, deny)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p.Config())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGrokWireContract(t *testing.T) {
	policyPath := writePolicy(t, permission.ModeDeny, []string{"Read(**)"}, nil, nil)

	t.Run("allow is empty output", func(t *testing.T) {
		var out bytes.Buffer
		payload := `{"hookEventName":"pre_tool_use","toolName":"read_file","toolInput":{"path":"README.md"}}`
		if err := Run(BackendGrok, policyPath, bytes.NewBufferString(payload), &out); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatalf("allow output = %q, want empty (native no-op allow)", out.String())
		}
	})

	t.Run("deny uses grok's exact blocking shape", func(t *testing.T) {
		var out bytes.Buffer
		payload := `{"hookEventName":"pre_tool_use","toolName":"run_terminal_command","toolInput":{"command":"rm -rf /tmp/x"}}`
		if err := Run(BackendGrok, policyPath, bytes.NewBufferString(payload), &out); err != nil {
			t.Fatal(err)
		}
		reason := permission.DenyMessage("run_terminal_command", map[string]any{"command": "rm -rf /tmp/x"}, "")
		want, _ := json.Marshal(map[string]any{"decision": "deny", "reason": reason})
		want = append(want, '\n')
		if !bytes.Equal(out.Bytes(), want) {
			t.Fatalf("wire output = %q\nwant %q", out.Bytes(), want)
		}
	})
}

func TestKimiWireContract(t *testing.T) {
	policyPath := writePolicy(t, permission.ModeDeny, []string{"Read(**)"}, nil, nil)
	var out bytes.Buffer
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo hello-from-kimi"}}`
	if err := Run(BackendKimi, policyPath, bytes.NewBufferString(payload), &out); err != nil {
		t.Fatal(err)
	}
	reason := permission.DenyMessage("Bash", map[string]any{"command": "echo hello-from-kimi"}, "")
	want, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"permissionDecision":       "deny",
			"permissionDecisionReason": reason,
		},
	})
	want = append(want, '\n')
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("wire output = %q\nwant %q", out.Bytes(), want)
	}
}

func TestMalformedPayloadFailsClosed(t *testing.T) {
	policyPath := writePolicy(t, permission.ModeDeny, nil, nil, nil)
	for _, backend := range []string{BackendGrok, BackendKimi} {
		t.Run(backend, func(t *testing.T) {
			var out bytes.Buffer
			if err := Run(backend, policyPath, bytes.NewBufferString("not-json"), &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out.Bytes(), []byte("deny")) {
				t.Fatalf("malformed request did not emit a deny verdict: %q", out.String())
			}
		})
	}
}
