//go:build clilive

// Real-CLI proof that the one-shot hook channel (#1672) is ACCEPTED and
// HONOURED by the claude CLI — gated by the `clilive` build tag because it
// needs `claude` on PATH, and no CI job runs it.
//
// This is the proof formatOutput's gate rests on: the engine registers the
// task's PreToolUse gate on a one-shot `--input-format stream-json` spawn, and
// a hook the CLI ignored there would be a paper bound where the code now
// claims a real one. Read (CLI 2.1.282, twice): the `initialize` is accepted
// (a `control_response success` for its request id) and PreToolUse FIRES.
// A refusal or a silent non-firing is a regression to report, not a design
// choice — formatOutput would be bounding a gated node's second spawn with
// nothing.
//
//	devbox run -- env -u ZAI_API_KEY -u ANTHROPIC_BASE_URL -u ANTHROPIC_AUTH_TOKEN \
//	  go test -tags clilive -run TestCLILiveOneShotHookChannel -v ./pkg/backend/delegate/
//
// The transport shape this exercises is pinned without the CLI by
// TestAHookedOneShotSpawnSendsItsInitializeOnStdin.
package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

func TestCLILiveOneShotHookChannelIsNotRefused(t *testing.T) {
	cli, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	var fired int
	var stderr []string
	var kinds []string

	var controlResponses []string
	_, promptErr := claudesdk.Prompt(ctx, "Use the Bash tool to run exactly `echo probe-ok`, then reply with the single word done.",
		claudesdk.WithCLIPath(cli),
		// A deliberately invalid key: the handshake and context assembly
		// happen, the model call does not. Nothing is spent.
		claudesdk.WithEnv("ANTHROPIC_API_KEY", "sk-ant-invalid-probe-0000000000000000"),
		claudesdk.WithEnv("ANTHROPIC_AUTH_TOKEN", ""),
		claudesdk.WithEnv("ANTHROPIC_BASE_URL", ""),
		claudesdk.WithMaxTurns(3),
		claudesdk.WithAllowedTools("Bash"),
		claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
			Handler: func(ctx context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
				fired++
				return claudesdk.HookOutput{Decision: "deny", DecisionReason: "probe"}, nil
			},
		}),
		claudesdk.WithMessageCallback(func(kind string, data json.RawMessage) {
			kinds = append(kinds, kind)
			if kind == "control_response" {
				// Only the verdict and the id it answers. The CLI's own
				// initialize response carries the whole local configuration
				// — every skill, agent, command and the account — and this
				// log is shared: never print the payload.
				var resp struct {
					Response struct {
						Subtype   string `json:"subtype"`
						RequestID string `json:"request_id"`
						Error     string `json:"error,omitempty"`
					} `json:"response"`
				}
				if err := json.Unmarshal(data, &resp); err != nil {
					controlResponses = append(controlResponses, "unparseable control_response")
					return
				}
				controlResponses = append(controlResponses,
					resp.Response.Subtype+" for "+resp.Response.RequestID+" "+resp.Response.Error)
			}
		}),
		claudesdk.WithStderrCallback(func(line string) {
			if line != "" {
				stderr = append(stderr, line)
			}
		}),
	)

	// The verdict. A typed refusal is the one answer that settles ARB-20
	// against wiring the gate here; anything else means the CLI took the
	// registration without complaint.
	refused := promptErr != nil && strings.Contains(promptErr.Error(), "refused the one-shot hook registration")

	t.Logf("one-shot hook channel against claude CLI at %s", cli)
	t.Logf("  refused the initialize: %v", refused)
	t.Logf("  Prompt error:           %v", promptErr)
	t.Logf("  message kinds seen:     %v", kinds)
	t.Logf("  PreToolUse fired:       %d", fired)
	for _, cr := range controlResponses {
		t.Logf("  control_response: %s", cr)
	}
	for _, l := range stderr {
		t.Logf("  stderr: %s", l)
	}

	if refused {
		t.Fatalf("the CLI REFUSES hooks on the one-shot stream, and formatOutput wires the task's gate onto exactly this spawn — a gated node's structured-output pass would be bounded by nothing: %v", promptErr)
	}

	// A CLI that does not understand the control protocol at all would die
	// before assembling anything; seeing its own messages proves the stream
	// was read rather than rejected.
	// The decisive read: the model attempted a tool, so if the CLI honours
	// hooks registered on this path the handler ran. If it did not, the
	// registration was accepted and ignored — a paper bound, and
	// formatOutput's gate would hold nothing.
	if fired == 0 {
		t.Errorf("the CLI accepted the registration but PreToolUse never fired on the one-shot path — formatOutput's travelling gate is a paper bound on this CLI version")
	} else {
		t.Logf("the CLI honours hooks on the one-shot path (%d PreToolUse callback(s)) — the travelling gate is a real bound", fired)
	}

	if len(kinds) == 0 {
		var pe *claudesdk.ProcessError
		if errors.As(promptErr, &pe) {
			t.Logf("  no messages: process error exit=%d stderr=%q", pe.ExitCode, pe.Stderr)
		}
		t.Fatalf("the CLI emitted no NDJSON message, so this run proves nothing about hook acceptance — read the stderr above before concluding anything")
	}
}

// The combination formatOutput actually produces, which the probe above does
// not exercise: the REAL gate AND a structured-output schema on one one-shot
// spawn. Hooks move the prompt from argv to stdin (`--input-format
// stream-json`), and `--json-schema` is what makes the pass worth running at
// all — a CLI that dropped the structured output under the stdin protocol
// would turn every gated schema'd node onto the text-parsing fallback,
// silently.
//
// It drives wireFormattingPermissionHook rather than a hand-rolled hook, so
// what it reads is the GUARANTEE and not a lookalike. Measured the hard way:
// a hand-rolled hook that denied every tool denied `StructuredOutput` too and
// the pass came back `is_error` with nothing — the policy's exemption for it
// (permission.IsInfrastructureTool) is load-bearing, and only the real gate
// carries it.
//
// This one needs a real model turn, so it spends a few cents on whatever
// login the CLI holds.
//
//	devbox run -- env -u ZAI_API_KEY -u ANTHROPIC_BASE_URL -u ANTHROPIC_AUTH_TOKEN -u ANTHROPIC_API_KEY \
//	  go test -tags clilive -count=1 -run TestCLILiveAHookedOneShotStillReturnsStructuredOutput -v ./pkg/backend/delegate/
func TestCLILiveAHookedOneShotStillReturnsStructuredOutput(t *testing.T) {
	cli, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	pol, err := permission.NewPolicy(permission.ModeDeny, nil, nil, []string{"Bash"})
	if err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"colour": map[string]any{"type": "string"}},
		"required":             []any{"colour"},
		"additionalProperties": false,
	}

	var stderr []string
	opts := []claudesdk.Option{
		claudesdk.WithCLIPath(cli),
		claudesdk.WithOutputFormat(schema),
		claudesdk.WithMaxTurns(3),
		claudesdk.WithStderrCallback(func(line string) {
			if line != "" {
				stderr = append(stderr, line)
			}
		}),
	}
	// The product's own wiring, not a stand-in for it.
	opts = wireFormattingPermissionHook(Task{NodeID: "probe", Permission: pol}, opts)

	rm, promptErr := claudesdk.Prompt(ctx, "Answer with the structured output schema: set colour to the single word blue. Call no other tool.", opts...)

	t.Logf("hooked one-shot with a schema, against claude CLI at %s", cli)
	t.Logf("  Prompt error:        %v", promptErr)
	if rm != nil {
		// The SHAPE, never the payload: a result can quote what the model read.
		t.Logf("  structured_output:   present=%v", rm.StructuredOutput != nil)
		resultLen := 0
		if rm.Result != nil {
			resultLen = len(*rm.Result)
		}
		t.Logf("  result length:       %d bytes, is_error=%v", resultLen, rm.IsError)
	}

	if promptErr != nil {
		t.Fatalf("the gated one-shot spawn failed with a schema attached — formatOutput runs exactly this shape: %v (stderr: %v)", promptErr, stderr)
	}
	if rm == nil {
		t.Fatal("no result message: the pass produced nothing to parse")
	}
	if rm.IsError {
		t.Errorf("the gated one-shot spawn came back is_error — a gated schema'd node would lose its finishing pass (stderr: %v)", stderr)
	}
	if rm.StructuredOutput == nil {
		t.Errorf("the CLI returned no structured_output on a GATED one-shot spawn, so a gated schema'd node would fall back to text parsing (stderr: %v)", stderr)
	}
}
