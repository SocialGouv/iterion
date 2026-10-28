//go:build clilive

// Real-CLI proof that the one-shot hook channel (#1672.1) is ACCEPTED by the
// claude CLI — gated by the `clilive` build tag because it needs `claude` on
// PATH. It costs nothing: the CLI authenticates with a deliberately invalid
// key, so it assembles its context, answers the control-protocol handshake,
// and only then gets a 401. The question this answers is the cheap half of
// ARB-20: does the CLI REFUSE an `initialize` carrying hooks on the one-shot
// `--input-format stream-json` path? A refusal settles #1672.2 as option B at
// zero cost. Acceptance does not by itself prove PreToolUse FIRES on a
// resumed pass — that is the paid half, and it needs a model turn.
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
		t.Fatalf("the CLI REFUSES hooks on the one-shot stream — #1672.2 takes option B (the withholding stays) and the gate must not be wired onto this spawn: %v", promptErr)
	}

	// A CLI that does not understand the control protocol at all would die
	// before assembling anything; seeing its own messages proves the stream
	// was read rather than rejected.
	// The decisive read of ARB-20: the model attempted a tool, so if the CLI
	// honours hooks registered on this path the handler ran. If it did not,
	// the registration was accepted and ignored — a paper bound, and #1672.2
	// stays option B.
	if fired == 0 {
		t.Errorf("ARB-20 reads B: the CLI accepted the registration but PreToolUse never fired on the one-shot path, so the gate must NOT replace the withholding in formatOutput")
	} else {
		t.Logf("ARB-20 reads A: the CLI honours hooks on the one-shot path (%d PreToolUse callback(s)) — the travelling gate is a real bound and #1672.2 is mechanical", fired)
	}

	if len(kinds) == 0 {
		var pe *claudesdk.ProcessError
		if errors.As(promptErr, &pe) {
			t.Logf("  no messages: process error exit=%d stderr=%q", pe.ExitCode, pe.Stderr)
		}
		t.Fatalf("the CLI emitted no NDJSON message, so this run proves nothing about hook acceptance — read the stderr above before concluding anything")
	}
}
