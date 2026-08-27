package permissionhook

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

func writePolicy(t *testing.T, mode permission.Mode, allow, ask, deny []string) string {
	t.Helper()
	p, err := permission.NewPolicy(mode, allow, ask, deny)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodePolicy(p.Config())
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestUndecodablePolicyFailsClosed pins the direction of the failure. The hook
// is the gate's whole authority on these backends, and both CLIs treat a
// broken hook as ALLOW, so anything the hook cannot understand must still
// produce an explicit deny rather than an error the CLI shrugs off.
func TestUndecodablePolicyFailsClosed(t *testing.T) {
	for _, backend := range []string{BackendGrok, BackendKimi} {
		for name, encoded := range map[string]string{
			"empty":        "",
			"not base64":   "!!!not-base64!!!",
			"not a policy": base64.RawURLEncoding.EncodeToString([]byte(`{"mode":`)),
		} {
			t.Run(backend+"/"+name, func(t *testing.T) {
				var out bytes.Buffer
				payload := `{"tool_name":"Bash","toolName":"run_terminal_command","tool_input":{},"toolInput":{}}`
				if err := Run(backend, encoded, bytes.NewBufferString(payload), &out); err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(out.Bytes(), []byte("deny")) {
					t.Fatalf("undecodable policy did not fail closed: %q", out.String())
				}
			})
		}
	}
}

func TestGrokWireContract(t *testing.T) {
	policyB64 := writePolicy(t, permission.ModeDeny, []string{"Read(**)"}, nil, nil)

	t.Run("allow is empty output", func(t *testing.T) {
		var out bytes.Buffer
		payload := `{"hookEventName":"pre_tool_use","toolName":"read_file","toolInput":{"path":"README.md"}}`
		if err := Run(BackendGrok, policyB64, bytes.NewBufferString(payload), &out); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatalf("allow output = %q, want empty (native no-op allow)", out.String())
		}
	})

	t.Run("deny uses grok's exact blocking shape", func(t *testing.T) {
		var out bytes.Buffer
		payload := `{"hookEventName":"pre_tool_use","toolName":"run_terminal_command","toolInput":{"command":"rm -rf /tmp/x"}}`
		if err := Run(BackendGrok, policyB64, bytes.NewBufferString(payload), &out); err != nil {
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
	policyB64 := writePolicy(t, permission.ModeDeny, []string{"Read(**)"}, nil, nil)
	var out bytes.Buffer
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo hello-from-kimi"}}`
	if err := Run(BackendKimi, policyB64, bytes.NewBufferString(payload), &out); err != nil {
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
	policyB64 := writePolicy(t, permission.ModeDeny, nil, nil, nil)
	for _, backend := range []string{BackendGrok, BackendKimi} {
		t.Run(backend, func(t *testing.T) {
			var out bytes.Buffer
			if err := Run(backend, policyB64, bytes.NewBufferString("not-json"), &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out.Bytes(), []byte("deny")) {
				t.Fatalf("malformed request did not emit a deny verdict: %q", out.String())
			}
		})
	}
}

// panickingReader makes Run blow up mid-flight, standing in for the class this
// guards: a pathological rule or an unanticipated input shape panicking inside
// the evaluator.
type panickingReader struct{}

func (panickingReader) Read([]byte) (int, error) { panic("boom") }

// TestPanicFailsClosed: a panic kills the process with empty stdout, and BOTH
// CLIs read that as ALLOW — so without recovering, any future bug in the
// evaluator becomes a silent gate bypass instead of a loud failure. The last
// thing this process does before dying must still be to deny.
func TestPanicFailsClosed(t *testing.T) {
	policyB64 := writePolicy(t, permission.ModeDeny, nil, nil, nil)
	for _, backend := range []string{BackendGrok, BackendKimi} {
		t.Run(backend, func(t *testing.T) {
			var out bytes.Buffer
			err := Run(backend, policyB64, panickingReader{}, &out)
			if err != nil {
				t.Fatalf("Run returned %v; a returned error is itself fail-open on both CLIs", err)
			}
			if !bytes.Contains(out.Bytes(), []byte("deny")) {
				t.Fatalf("panic did not fail closed: %q", out.String())
			}
		})
	}
}
