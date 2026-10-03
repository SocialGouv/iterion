package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// toolErrorWire builds the production pair: the store event hooks writing to
// a run log, and the bridge a claude_code delegate receives as Task.Hooks
// (delegateHooksFor), both on one guard.
func toolErrorWire(t *testing.T, g *secretguard.Guard) (delegate.TaskHooks, *bytes.Buffer, *captureEmitter) {
	t.Helper()
	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &logBuf)
	em := &captureEmitter{}
	hooks := NewStoreEventHooks(context.Background(), em, "run-1", logger, g)
	e := newFallbackExecutor(delegate.NewRegistry(), hooks)
	e.secretGuard = g
	e.logger = logger
	return e.delegateHooksFor("deploy", delegate.BackendClaudeCode, 1), &logBuf, em
}

// A claude_code tool that fails while printing the value it ran with (curl -v
// dumping its request headers, a database refusing a password): the tool
// error line of the run log, and the tool_error event, carry the output in
// placeholder form — a secret the 500-byte cut splits included.
func TestAToolErrorNeverCarriesASecret(t *testing.T) {
	const token = "s3cr3t-VALUE-7f6e5d4c3b2a"
	const pw = "correct horse battery staple!9f8e"
	const long = "Tr0ub4dor-and-3-horses-correct-battery-staple-9f8e7d6c5b4a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_TOKEN", Value: token}, {Name: "DB_PASSWORD", Value: pw}, {Name: "DEPLOY_PASSWORD", Value: long}}, secretguard.DefaultConfig())
	for name, c := range map[string]struct{ out, secret string }{
		"a token in curl's headers":   {"> GET /deploy HTTP/2\n> Authorization: Bearer " + token + "\n< HTTP/2 401\ncurl: (22) 401", token},
		"a passphrase in an error":    {"FATAL:  password authentication failed\nDETAIL: attempted password \"" + pw + "\"", pw},
		"a secret across the 500 cut": {strings.Repeat("x", 480) + long + " rest", long[:16]},
	} {
		th, logBuf, em := toolErrorWire(t, g)
		th.OnToolStarted("Bash", "tu1", json.RawMessage(`{"command":"deploy"}`))
		th.OnToolCalled("Bash", "tu1", true, c.out)
		if strings.Contains(logBuf.String(), c.secret) {
			t.Errorf("%s: the run log carries it: %s", name, logBuf.String())
		}
		evs, _ := json.Marshal(em.events)
		if strings.Contains(string(evs), c.secret) {
			t.Errorf("%s: an event carries it: %s", name, evs)
		}
	}
}

// The rejected input shown beside a tool error is redacted before its
// 600-byte cut: a secret straddling it is recognised whole.
func TestARejectedInputNeverCarriesASecret(t *testing.T) {
	const secret = "Tr0ub4dor-and-3-horses-correct-battery-staple-9f8e7d6c5b4a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_PASSWORD", Value: secret}}, secretguard.DefaultConfig())
	th, logBuf, _ := toolErrorWire(t, g)
	prefix := `{"command":"` + strings.Repeat("x", 600-len(`{"command":"`)-20)
	th.OnToolStarted("Bash", "tu4", json.RawMessage(prefix+secret+`"}`))
	th.OnToolCalled("Bash", "tu4", true, "boom")
	if !strings.Contains(logBuf.String(), "rejected input") {
		t.Fatalf("scenario broken: no rejected input in %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), secret[:16]) {
		t.Fatalf("the rejected-input preview carries a head of the secret: %s", logBuf.String())
	}
}

// Whatever produced a tool error, its text reaches the run log and the
// tool_error event redacted: the sink redacts too.
func TestAToolErrorFromAnyProducerIsRedactedAtTheSink(t *testing.T) {
	const pw = "correct horse battery staple!9f8e"
	g := secretguard.New([]secretguard.Secret{{Name: "DB_PASSWORD", Value: pw}}, secretguard.DefaultConfig())
	var logBuf bytes.Buffer
	em := &captureEmitter{}
	hooks := NewStoreEventHooks(context.Background(), em, "run-1", iterlog.New(iterlog.LevelInfo, &logBuf), g)
	hooks.OnToolCall("deploy", LLMToolCallInfo{ToolName: "Bash", ToolUseID: "tu1", Error: errors.New("attempted password \"" + pw + "\"")})
	evs, _ := json.Marshal(em.events)
	if strings.Contains(logBuf.String(), pw) || strings.Contains(string(evs), pw) {
		t.Fatalf("run log = %s, events = %s", logBuf.String(), evs)
	}
	if !strings.Contains(logBuf.String(), "Tool error") {
		t.Fatalf("scenario broken: no tool error line in %s", logBuf.String())
	}
}

// a known secret longer than the 1 KiB floor, straddling the
// rejected-input preview's 600-byte bound, is recognised whole.
func TestALongSecretAcrossTheRejectedInputCutIsRecognised(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 3000; i++ {
		fmt.Fprintf(&b, "correct-horse-%04d-", i)
	}
	secret := b.String()
	g := secretguard.New([]secretguard.Secret{{Name: "BIG", Value: secret}}, secretguard.DefaultConfig())
	th, logBuf, _ := toolErrorWire(t, g)
	prefix := `{"command":"` + strings.Repeat("x", 600-len(`{"command":"`)-20)
	th.OnToolStarted("Bash", "tu9", json.RawMessage(prefix+secret+`"}`))
	th.OnToolCalled("Bash", "tu9", true, "boom")
	if !strings.Contains(logBuf.String(), "rejected input") {
		t.Fatalf("scenario broken: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "correct-horse-0000") {
		t.Fatalf("the rejected-input preview carries a head of a %d-byte secret: %s", len(secret), logBuf.String())
	}
}

// with no known value, a token shape the heuristic recognises
// whole — and not in part — straddling the 600-byte bound is not shown in part.
func TestAnUnknownTokenAcrossTheRejectedInputCutIsNotShownInPart(t *testing.T) {
	const tok = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	g := secretguard.New(nil, secretguard.DefaultConfig())
	th, logBuf, _ := toolErrorWire(t, g)
	prefix := `{"command":"` + strings.Repeat("x", 600-len(`{"command":"`)-11) + " "
	th.OnToolStarted("Bash", "tu8", json.RawMessage(prefix+tok+`"}`))
	th.OnToolCalled("Bash", "tu8", true, "boom")
	if !strings.Contains(logBuf.String(), "rejected input") {
		t.Fatalf("scenario broken: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), tok[:10]) {
		t.Fatalf("the rejected-input preview carries a head of an unknown token: %s", logBuf.String())
	}
}

// A failing tool node writes a tool error line from each of its two hooks:
// both, and the events, carry the error in placeholder form.
func TestAToolNodesErrorLinesAreRedacted(t *testing.T) {
	const key = "Tr0ub4dor-and-3-horses-correct-battery-staple-9f8e7d6c5b4a"
	g := secretguard.New([]secretguard.Secret{{Name: "API_KEY", Value: key}}, secretguard.DefaultConfig())
	var logBuf bytes.Buffer
	em := &captureEmitter{}
	h := NewStoreEventHooks(context.Background(), em, "run-1", iterlog.New(iterlog.LevelInfo, &logBuf), g)
	err := errors.New("mcp: tools/call search: 401 Unauthorized (api key " + key + " rejected)")
	h.OnToolCall("fetch", LLMToolCallInfo{ToolName: "mcp__vendor__search", Duration: time.Millisecond, Error: err})
	h.OnToolNodeResult("fetch", "mcp__vendor__search", []byte(`{"q":"x"}`), "", time.Millisecond, err)
	evs, _ := json.Marshal(em.events)
	if strings.Count(logBuf.String(), "Tool error") != 2 {
		t.Fatalf("scenario broken: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), key) || strings.Contains(string(evs), key) {
		t.Fatalf("run log = %s", logBuf.String())
	}
}

// A claude_code tool error whose known secret is longer than the bridge's
// 1 KiB floor straddles its 500-byte cut: recognised whole, never shown in
// part.
func TestALongSecretAcrossTheToolErrorCutIsRecognised(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 3000; i++ {
		fmt.Fprintf(&b, "correct-horse-%04d-", i)
	}
	secret := b.String()
	g := secretguard.New([]secretguard.Secret{{Name: "BIG", Value: secret}}, secretguard.DefaultConfig())
	th, logBuf, em := toolErrorWire(t, g)
	th.OnToolStarted("Bash", "tu1", json.RawMessage(`{"command":"deploy"}`))
	th.OnToolCalled("Bash", "tu1", true, strings.Repeat("x", 480)+secret+" rest")
	if !strings.Contains(logBuf.String(), "Tool error") {
		t.Fatalf("scenario broken: %s", logBuf.String())
	}
	evs, _ := json.Marshal(em.events)
	if strings.Contains(logBuf.String(), "correct-horse-0000") || strings.Contains(string(evs), "correct-horse-0000") {
		t.Fatalf("a head of a %d-byte secret reached the run log or an event: %s", len(secret), logBuf.String())
	}
}

// With no known value, a token shape the heuristic recognises only whole
// straddles the bridge's 500-byte cut: not shown in part.
func TestAnUnknownTokenAcrossTheToolErrorCutIsNotShownInPart(t *testing.T) {
	const tok = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	g := secretguard.New(nil, secretguard.DefaultConfig())
	th, logBuf, _ := toolErrorWire(t, g)
	th.OnToolStarted("Bash", "tu2", json.RawMessage(`{"command":"deploy"}`))
	th.OnToolCalled("Bash", "tu2", true, strings.Repeat("x", 490)+" "+tok+" rest")
	if !strings.Contains(logBuf.String(), "Tool error") {
		t.Fatalf("scenario broken: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), tok[:9]) {
		t.Fatalf("the tool error line shows a head of an unknown token: %s", logBuf.String())
	}
}

const nodeErrTok = "tok-NODEERR-9f8e7d6c5b4a3210"

func nodeErrGuard() *secretguard.Guard {
	return secretguard.New([]secretguard.Secret{{Name: "TOK", Value: nodeErrTok}}, secretguard.DefaultConfig())
}

// A failing tool node's error quotes the command's output: what the engine
// persists (run.json), logs and posts carries the placeholder.
func TestAFailingShellToolNodesErrorCarriesNoSecret(t *testing.T) {
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(t.TempDir()), WithSecretGuard(nodeErrGuard()))
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "leak"},
		Command:  "echo auth=__ITERION_SECRET_TOK__; echo err=__ITERION_SECRET_TOK__ >&2; exit 3",
	}
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("scenario broken: err = %v", err)
	}
	if strings.Contains(err.Error(), nodeErrTok) {
		t.Fatalf("the node error handed to the engine carries the value: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "__ITERION_SECRET_TOK__") {
		t.Fatalf("the node error no longer quotes the output: %q", err.Error())
	}
}

// A tool node calling a registry (MCP) tool that fails echoing its key: the
// node error carries the placeholder.
func TestARegistryToolNodesErrorCarriesNoSecret(t *testing.T) {
	tr := tool.NewRegistry()
	_ = tr.RegisterBuiltin("vendor_search", "search", nil, func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", errors.New("mcp: tools/call search: 401 Unauthorized (api key " + nodeErrTok + " rejected)")
	})
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(NewRegistry(), wf, WithToolRegistry(tr), WithSecretGuard(nodeErrGuard()))
	_, err := exec.Execute(context.Background(), &ir.ToolNode{BaseNode: ir.BaseNode{ID: "fetch"}, Command: "vendor_search"}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("scenario broken: err = %v", err)
	}
	if strings.Contains(err.Error(), nodeErrTok) {
		t.Fatalf("the node error handed to the engine carries the value: %q", err.Error())
	}
}

// The scrubbed error keeps what the engine classifies (errors.Is /
// errors.As see what the node returned), and no link a reporter walking the
// chain would print carries the value.
func TestAScrubbedNodeErrorKeepsItsClassNotItsText(t *testing.T) {
	sentinel := errors.New("rate limited")
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithSecretGuard(nodeErrGuard()))
	err := exec.scrubNodeError(fmt.Errorf("call with %s: %w", nodeErrTok, &os.PathError{Op: "open", Path: "x", Err: sentinel}))
	var pathErr *os.PathError
	if !errors.Is(err, sentinel) || !errors.As(err, &pathErr) {
		t.Fatalf("err = %q: errors.Is(sentinel) = %v, errors.As(*os.PathError) = %v", err.Error(), errors.Is(err, sentinel), errors.As(err, &pathErr))
	}
	for link := err; link != nil; link = errors.Unwrap(link) {
		if strings.Contains(link.Error(), nodeErrTok) {
			t.Fatalf("a link of the chain (%T) carries the value: %q", link, link.Error())
		}
	}
}

// A verified action's self-repair asks a model to fix the command: the
// output it quotes — stdout and stderr — goes back to placeholders, like the
// command itself; the postcondition error the node ends on too.
func TestTheSelfRepairPromptCarriesNoSecret(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-funding")
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "structured_output", `{"corrected_command":"true"}`, 50, 10))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(t.TempDir()), WithSecretGuard(nodeErrGuard()))
	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "va"},
		Command:       "curl_like() { echo \"token=$1\"; echo \"> Authorization: Bearer $1\" >&2; return 22; }; curl_like __ITERION_SECRET_TOK__",
		Goal:          "call the API",
		Postcondition: "false",
		Policy:        ir.PolicyRecover,
		Recovery:      &ir.RecoverySpec{MaxRepairAttempts: 1},
	}
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	calls := mock.getCalls()
	if len(calls) == 0 {
		t.Fatal("scenario broken: the self-repair never called the model")
	}
	prompt, _ := json.Marshal(calls[0].Messages)
	if strings.Contains(string(prompt), nodeErrTok) {
		t.Fatalf("the self-repair prompt sent to the model carries the value: %s", prompt)
	}
	for _, quoted := range []string{"token=__ITERION_SECRET_TOK__", "Authorization: Bearer __ITERION_SECRET_TOK__"} {
		if !strings.Contains(string(prompt), quoted) {
			t.Fatalf("the self-repair prompt does not quote %q: %s", quoted, prompt)
		}
	}
	if err == nil || strings.Contains(err.Error(), nodeErrTok) {
		t.Fatalf("err = %v: the postcondition error carries the value", err)
	}
}

// A node error quoting a token no secret registered — a command printing a
// credential from its environment — has it redacted by the sink heuristic.
func TestANodeErrorsUnknownTokenIsRedacted(t *testing.T) {
	const tok = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(t.TempDir()), WithSecretGuard(nodeErrGuard()))
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "leak"}, Command: "echo token=" + tok + " >&2; exit 3"}
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("scenario broken: err = %v", err)
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatalf("the node error handed to the engine carries an unknown token: %q", err.Error())
	}
}
