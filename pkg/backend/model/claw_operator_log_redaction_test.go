package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sinkRedaction is the run's Layer-0 sink redaction for one known value — the
// shape the executor builds from the secret guard and hands every task as
// delegate.Task.RedactSecrets.
func sinkRedaction(secret, placeholder string) func(string) string {
	return func(s string) string { return strings.ReplaceAll(s, secret, placeholder) }
}

// The container's stderr is text from the far side of the sandbox boundary,
// and it reaches two sinks: the node's error, which the executor's guard
// scrubs on its way out (ClawExecutor.scrubNodeError), and the operator's log,
// which is this backend's own duty. A run whose secrets are known must not
// read one of them in clear.
func TestSandboxDispatch_RedactsTheRunnersStderrForTheOperatorLog(t *testing.T) {
	t.Parallel()
	const secret = "sk-ant-api03-WITNESS-0123456789abcdefghijklmnopqrstuvwxyz"
	const placeholder = "__ITERION_SECRET_api_key__"

	dir := t.TempDir()
	resEnv, err := delegate.NewResultEnvelope(delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(resEnv)
	if err != nil {
		t.Fatal(err)
	}
	resultFile := filepath.Join(dir, "result.ndjson")
	if err := os.WriteFile(resultFile, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// The shape runClawRunner prints when the node's own error quotes the call
	// it made (cmd/iterion/claw_runner.go).
	diagnostic := "iterion-claw-runner: anthropic: 401 unauthorized (x-api-key: " + secret + ")"
	var logs strings.Builder
	b := loggedTestClawBackend(&logs)
	res, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox:       newAnsweringSandbox(t, diagnostic, resultFile),
		RedactSecrets: sinkRedaction(secret, placeholder),
	})
	if err != nil {
		t.Fatalf("the node was failed by a diagnostic it answered through: %v", err)
	}
	if res.Output["verdict"] != "ok" {
		t.Fatalf("output = %v, want the runner's answer", res.Output)
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the runner's stderr reached the operator log with the run's secret in clear: %q", logs.String())
	}
	if !strings.Contains(logs.String(), placeholder) {
		t.Errorf("the runner's stderr is unreported, or reported outside the run's redaction: %q", logs.String())
	}
}

// A refused forwarded call is recorded on the launcher's side with the
// ARGUMENT the agent wrote — permission.DenyMessage quotes it — so an agent
// that put a secret value in a denied call writes it into the operator's log
// unless the log redacts it. Since #2458 that pass is the run logger's
// chokepoint (WithRedactor), which BuildExecutor wires from the run's guard.
func TestClawLauncher_RedactsARefusedForwardedCallForTheOperatorLog(t *testing.T) {
	t.Parallel()
	const secret = "ghp_WITNESS0123456789abcdefghijklmnopqrstuv"
	const placeholder = "__ITERION_SECRET_deploy_token__"

	pol, err := permission.NewPolicy(permission.ModeDeny, []string{"*"}, nil, []string{"mcp_github_create_issue"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	var logs strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelInfo, &logs).WithRedactor(sinkRedaction(secret, placeholder))))
	task := delegate.Task{
		NodeID: "deploy", Iteration: 1, Permission: pol,
		ToolDefs: []delegate.ToolDef{{
			Name:    "mcp_github_create_issue",
			Execute: func(context.Context, json.RawMessage) (string, error) { return "", nil },
		}},
		RedactSecrets: sinkRedaction(secret, placeholder),
	}
	input := json.RawMessage(`{"body":"deploy token: ` + secret + `"}`)
	handler, _ := b.multiplexerHandler(context.Background(), task, sessionSeed{})
	if _, err := handler.OnToolCall(context.Background(), "mcp_github_create_issue", input); err == nil {
		t.Fatal("a denied forwarded call was not refused — this witness no longer reaches the refusal it watches")
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the refused call's argument reached the operator log with the run's secret in clear: %q", logs.String())
	}
	if !strings.Contains(logs.String(), placeholder) {
		t.Errorf("the refusal is unreported, or reported outside the run's redaction: %q", logs.String())
	}
}

// A relayed event this host cannot decode is reported with the decoder's
// error, and that error quotes the payload the runner sent whenever the field
// parses its own text (a timestamp). The payload crossed the boundary, and the
// report is redacted by the run logger's chokepoint (#2458) — the pass the
// production run gets from BuildExecutor wiring the guard onto the logger.
func TestClawRelay_RedactsAnUndecodableEventForTheOperatorLog(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-WITNESS0123456789abcdefghijklmnop"
	const placeholder = "__ITERION_SECRET_openai_key__"

	var logs strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelWarn, &logs).WithRedactor(sinkRedaction(secret, placeholder))))
	task := delegate.Task{
		NodeID: "plan_review", Iteration: 1,
		RedactSecrets: sinkRedaction(secret, placeholder),
	}
	handler, _ := b.multiplexerHandler(context.Background(), task, sessionSeed{})
	handler.OnEvent(string(store.EventLLMRequest), map[string]any{"timestamp": secret})
	t.Logf("operator log: %q", logs.String())
	if logs.Len() == 0 {
		t.Fatal("an undecodable relayed event is reported to nobody — this witness no longer reaches the report it watches")
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the relayed payload reached the operator log with the run's secret in clear: %q", logs.String())
	}
	if !strings.Contains(logs.String(), placeholder) {
		t.Errorf("the decode failure is reported outside the run's redaction: %q", logs.String())
	}
}

// A relayed event re-enters the host as an event, so Layer 0 scrubs the event
// copy at the emitter, and the LOG line is the run logger's chokepoint
// (#2458): the names the far side chose ride that line beside the content —
// the model the container says it called, the name it gave each tool call —
// and a half-redacted line, the error scrubbed and the name in clear, is the
// shape that hides a leak. Driven through the real chain the sibling retry
// witness uses: the crossing's event handler, the relay decoder, the
// production store hooks, and the logger wired as BuildExecutor wires it —
// the run's own guard on the run's own logger.
//
// A relayed tool call is four far-side values at three records, not one name:
// the name, the one-line detail [tooldisplay.HeaderDetail] reads out of the
// input, the tool-input block [tooldisplay.BlockBody] builds from it, and the
// raw input the DEBUG record quotes whole. The input therefore has to be the
// JSON OBJECT the wire form decodes (a JSON string relays on as a quoted
// scalar, which both readers refuse — measured: no detail on the name's line
// and no input block at all), its argument has to exceed the header's one-line
// budget so the block body is not empty, and the logger has to keep DEBUG,
// the floor at which the raw input is written. Each record this row needs is
// named in reach: a scenario that stops producing one of them is a witness
// that stopped watching a sink, and says so instead of passing.
func TestClawRelay_RedactsTheNamesOfARelayedEventForTheOperatorLog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		secret  string
		event   store.EventType
		payload func(secret string) map[string]any
		reach   []string
	}{
		{
			name:   "the model a relayed llm_request says it called",
			secret: "sk-proj-WITNESSMODEL0123456789abcdefghijk",
			event:  store.EventLLMRequest,
			payload: func(secret string) map[string]any {
				return map[string]any{
					"model": "openai/gpt-5?key=" + secret, "message_count": 2,
					"tool_count": 0, "reasoning_effort": "high-" + secret,
				}
			},
			reach: []string{"LLM call"},
		},
		{
			name:   "the name, detail, input block and raw input of a relayed llm_step_finished",
			secret: "sk-proj-WITNESSSTEP0123456789abcdefghijkl",
			event:  store.EventLLMStepFinished,
			payload: func(secret string) map[string]any {
				return map[string]any{"step": 1, "tool_calls": []any{
					map[string]any{
						"name": "bash(export KEY=" + secret + ")",
						"input": map[string]any{
							"command": "export KEY=" + secret + " && deploy --to " +
								strings.Repeat("prod/", 40),
						},
					},
				}}
			},
			reach: []string{"bash(export KEY=", "&& deploy --to", "tool input ", "raw input "},
		},
		{
			name:   "the tool name a relayed tool_called reports as failed",
			secret: "sk-proj-WITNESSTOOL0123456789abcdefghijkl",
			event:  store.EventToolCalled,
			payload: func(secret string) map[string]any {
				return map[string]any{
					"tool":  "mcp_deploy_push(token=" + secret + ")",
					"error": "boom", "input_size": 3, "duration_ms": 1,
				}
			},
			reach: []string{"Tool error"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: tc.secret}},
				secretguard.DefaultConfig())
			placeholder := g.Redact(tc.secret)
			if placeholder == tc.secret {
				t.Fatalf("the guard does not know the witness value — this witness no longer watches a redaction")
			}
			var logs strings.Builder
			logger := iterlog.New(iterlog.LevelDebug, &logs).WithRedactor(g.Redact)
			b := NewClawBackend(NewRegistry(),
				NewStoreEventHooks(context.Background(), &captureEmitter{}, "run-relay-names", logger, g, nil),
				RetryPolicy{}, WithClawLogger(logger))

			handler, _ := b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "plan", Iteration: 1}, sessionSeed{})
			handler.OnEvent(string(tc.event), tc.payload(tc.secret))

			for _, want := range tc.reach {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("the relayed event logged no %q record — this witness no longer reaches the sink it watches: %q", want, logs.String())
				}
			}
			if strings.Contains(logs.String(), tc.secret) {
				t.Errorf("a relayed name reached the operator log with the run's secret in clear: %q", logs.String())
			}
			if !strings.Contains(logs.String(), placeholder) {
				t.Errorf("the relayed name is reported outside the run's redaction: %q", logs.String())
			}
		})
	}
}

// The other branch of the same sink: an event type this host does not consume
// is reported at DEBUG with the TYPE the container sent — a name the far side
// chose, scrubbed by the run logger's chokepoint like the decoder's complaint
// above.
func TestClawRelay_RedactsAnUnconsumedEventTypeForTheOperatorLog(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-WITNESS0123456789abcdefghijklmnop"
	const placeholder = "__ITERION_SECRET_openai_key__"

	var logs strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelDebug, &logs).WithRedactor(sinkRedaction(secret, placeholder))))
	task := delegate.Task{
		NodeID: "plan_review", Iteration: 1,
		RedactSecrets: sinkRedaction(secret, placeholder),
	}
	handler, _ := b.multiplexerHandler(context.Background(), task, sessionSeed{})
	handler.OnEvent("vendor_metric_"+secret, nil)
	t.Logf("operator log: %q", logs.String())
	if logs.Len() == 0 {
		t.Fatal("an unconsumed relayed event is reported to nobody — this witness no longer reaches the report it watches")
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the relayed event type reached the operator log with the run's secret in clear: %q", logs.String())
	}
	if !strings.Contains(logs.String(), placeholder) {
		t.Errorf("the event type is reported outside the run's redaction: %q", logs.String())
	}
}

// Ask the sink before the pass — at every far-side record of a crossing log
// line, not just the one the stderr report builds.
//
// Since #2458 the pass is the run LOGGER's redactor, not a per-site helper:
// pkg/log drops a record above the logger's level before it is formatted AND
// before it is redacted, so an operator log that keeps nothing pays no pass
// over text the container chose. The three sites below are independent, so
// each subtest reddens alone — and each drives its line through a logger
// wired as BuildExecutor wires the run's, which is the wiring under test.
func TestClawCrossing_AnOperatorLogThatDropsTheRecordCostsNoPass(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-WITNESSLAZY0123456789abcdefghijk"

	pol, err := permission.NewPolicy(permission.ModeDeny, []string{"*"}, nil, []string{"mcp_github_create_issue"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	for _, tc := range []struct {
		name string
		// drops is a logger level that discards the line this site writes;
		// keeps is one that keeps it, so the same drive proves the subtest
		// reaches the sink at all.
		drops, keeps iterlog.Level
		drive        func(t *testing.T, b *ClawBackend, task delegate.Task)
	}{
		{
			name:  "an unconsumed relayed event, reported at debug",
			drops: iterlog.LevelInfo,
			keeps: iterlog.LevelDebug,
			drive: func(t *testing.T, b *ClawBackend, task delegate.Task) {
				handler, _ := b.multiplexerHandler(context.Background(), task, sessionSeed{})
				handler.OnEvent("vendor_metric_"+secret, nil)
			},
		},
		{
			name:  "a relayed event this host cannot decode, reported at warn",
			drops: iterlog.LevelError,
			keeps: iterlog.LevelWarn,
			drive: func(t *testing.T, b *ClawBackend, task delegate.Task) {
				handler, _ := b.multiplexerHandler(context.Background(), task, sessionSeed{})
				handler.OnEvent(string(store.EventLLMRequest), map[string]any{"timestamp": secret})
			},
		},
		{
			name:  "a refused forwarded call, reported at warn",
			drops: iterlog.LevelError,
			keeps: iterlog.LevelWarn,
			drive: func(t *testing.T, b *ClawBackend, task delegate.Task) {
				task.Permission = pol
				task.ToolDefs = []delegate.ToolDef{{
					Name:    "mcp_github_create_issue",
					Execute: func(context.Context, json.RawMessage) (string, error) { return "", nil },
				}}
				input := json.RawMessage(`{"body":"token: ` + secret + `"}`)
				handler, _ := b.multiplexerHandler(context.Background(), task, sessionSeed{})
				if _, err := handler.OnToolCall(context.Background(), "mcp_github_create_issue", input); err == nil {
					t.Fatal("a denied forwarded call was not refused — this witness no longer reaches the refusal it watches")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drive := func(level iterlog.Level) int {
				var passes int
				var logs strings.Builder
				redactor := func(s string) string {
					passes++
					return strings.ReplaceAll(s, secret, "__ITERION_SECRET_openai_key__")
				}
				b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
					WithClawLogger(iterlog.New(level, &logs).WithRedactor(redactor)))
				tc.drive(t, b, delegate.Task{
					NodeID: "plan_review", Iteration: 1,
					RedactSecrets: redactor,
				})
				return passes
			}
			if kept := drive(tc.keeps); kept == 0 {
				t.Fatalf("the sink that KEEPS this line asked for no pass — this witness no longer reaches the site it watches")
			}
			if dropped := drive(tc.drops); dropped != 0 {
				t.Errorf("an operator log that drops the record still paid %d pass(es) over the far side's text", dropped)
			}
		})
	}
}

// Which of the two arms a type the FAR SIDE chose can reach. The relay's
// decode failure is raised from inside a case on one of this host's own event
// constants and from nowhere else, so the warn line's type is always a host
// literal; a spelling this host does not know falls through to handled=false,
// err=nil — the DEBUG arm, the one the witness above pins the redaction on.
// Driven over the spellings a container could send, against a payload no host
// decoder accepts.
func TestClawRelay_OnlyAHostEventTypeReachesTheDecodeFailureArm(t *testing.T) {
	t.Parallel()
	breaks := map[string]any{"timestamp": "sk-proj-NOTATIME0123456789abcdefghijkl"}
	for _, typ := range []string{
		"vendor_metric_sk-proj-SECRET", "sk-proj-SECRET",
		"llm_request_sk-proj-SECRET", "LLM_REQUEST", "llm_turn_capture_x",
	} {
		handled, err := ApplyRelayedEvent(EventHooks{}, "n", typ, breaks)
		if handled || err != nil {
			t.Errorf("a far-side type %q reached the decode-failure arm (handled=%v, err=%v) — the warn line quotes a type the far side chose, and its redaction is load-bearing",
				typ, handled, err)
		}
	}
	// And a host constant DOES reach it, so the arm is not dead.
	if handled, err := ApplyRelayedEvent(EventHooks{}, "n", string(store.EventLLMRequest), breaks); !handled || err == nil {
		t.Fatalf("no host event type reaches the decode-failure arm (handled=%v, err=%v) — this witness watches nothing", handled, err)
	}
}
