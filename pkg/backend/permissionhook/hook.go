// Package permissionhook adapts third-party CLI PreToolUse payloads to
// iterion's shared permission.Policy evaluator.
package permissionhook

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

const (
	BackendGrok = "grok"
	BackendKimi = "kimi"
)

// Supports reports whether this hook can spell a blocking verdict in the
// named backend's dialect.
//
// Callers registering a hook MUST consult it. Run cannot fail closed on an
// unknown backend — a deny has to be written in SOME dialect, and it has none
// — so it returns an error, which both CLIs read as a non-zero exit with empty
// stdout, which both CLIs treat as ALLOW. The only place that mismatch can be
// caught safely is before the CLI is launched (PR #498 review, Rd8d86a).
func Supports(backend string) bool {
	return backend == BackendGrok || backend == BackendKimi
}

type event struct {
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	GrokToolName  string         `json:"toolName"`
	GrokToolInput map[string]any `json:"toolInput"`
}

// Run reads one native hook event, evaluates it, and writes the backend's
// exact blocking dialect. Malformed input and an undecodable policy fail
// closed by emitting a deny verdict; returning an error alone would make both
// CLIs fail open.
//
// The policy arrives BY VALUE (base64 of a permission.PolicyConfig), not by
// path. That is the security property, not an encoding preference: the CLI
// freezes its hook registration — and therefore this argv — when the session
// starts, so the gated agent cannot reach the gate's own authority mid-run.
// A file would be re-read on every call from a directory the agent runs as
// the same uid against, and one allowed write of `{"mode":"off"}` would
// disarm every later tool call (PR #498 review, finding R3e6bb0).
func Run(backend, policyB64 string, in io.Reader, out io.Writer) (err error) {
	if backend != BackendGrok && backend != BackendKimi {
		return fmt.Errorf("permission hook: unsupported backend %q", backend)
	}

	// A panic anywhere below — a pathological rule, an input shape Evaluate
	// did not anticipate — would kill this process with empty stdout, which
	// BOTH CLIs read as allow. That turns any future bug in the evaluator into
	// a silent gate bypass rather than a loud failure, so the last thing this
	// process does before dying is still to deny. Recovering here is a
	// security decision, not error hygiene: the panic is re-surfaced on stderr,
	// which is the hook's own feedback channel on both CLIs.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "permission hook: panic while evaluating the policy: %v\n", r)
			err = writeDeny(backend, out, "iterion's permission gate failed while evaluating this call; the tool call was blocked")
		}
	}()

	policy, err := decodePolicy(policyB64)
	if err != nil {
		return writeDeny(backend, out, "iterion could not load the permission policy; the tool call was blocked")
	}
	raw, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil {
		return writeDeny(backend, out, "iterion could not read the permission request; the tool call was blocked")
	}
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return writeDeny(backend, out, "iterion could not decode the permission request; the tool call was blocked")
	}
	toolName, toolInput := ev.ToolName, ev.ToolInput
	if backend == BackendGrok {
		toolName, toolInput = ev.GrokToolName, ev.GrokToolInput
	}
	if strings.TrimSpace(toolName) == "" {
		return writeDeny(backend, out, "iterion received a permission request without a tool name; the tool call was blocked")
	}
	if toolInput == nil {
		toolInput = map[string]any{}
	}

	decision, rule := policy.Evaluate(toolName, toolInput)
	switch decision {
	case permission.Allow:
		return nil
	case permission.Ask:
		return writeDeny(backend, out, fmt.Sprintf("%s needs operator approval, which %s cannot pause for; the tool call was blocked", toolName, backend))
	default:
		return writeDeny(backend, out, permission.DenyMessage(toolName, toolInput, rule))
	}
}

// EncodePolicy renders a policy into the argv-safe form Run decodes. It is
// exported so the delegate that builds the hook registration and the hook that
// reads it can never drift apart on the encoding.
func EncodePolicy(cfg permission.PolicyConfig) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodePolicy(encoded string) (*permission.Policy, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, err
	}
	var cfg permission.PolicyConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return permission.NewPolicyFromConfig(cfg)
}

func writeDeny(backend string, out io.Writer, reason string) error {
	enc := json.NewEncoder(out)
	switch backend {
	case BackendGrok:
		return enc.Encode(map[string]any{
			"decision": "deny",
			"reason":   reason,
		})
	case BackendKimi:
		return enc.Encode(map[string]any{
			"hookSpecificOutput": map[string]any{
				"permissionDecision":       "deny",
				"permissionDecisionReason": reason,
			},
		})
	default:
		return fmt.Errorf("permission hook: unsupported backend %q", backend)
	}
}
