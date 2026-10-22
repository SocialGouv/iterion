package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The structured-output contract of a claw node served by an
// OpenAI-compatible gateway (#2651), driven end to end: the REAL claw backend
// (ClawBackend.Execute with the registry's gateway factory, i.e.
// compatgw.NewClient and claw's chat-completions client) talks over loopback
// to a fake gateway that answers like the models measured behind the PLIAGE
// LiteLLM gateway. Each case is one way a model can fail the forced
// `structured_output` turn; the node must still come back with its verdict.

// gwVerdict is the answer every case's model means to give.
const gwVerdict = `{"batch_id":"b1","verdicts":[{"id":"1.1","verdict":"C"}],"notes":"ok"}`

// gwModelSpec is the node's model: a gateway route whose catalog entry the
// operator table declares (OPENAI_COMPATIBLE_MODELS below).
const gwModelSpec = "openai_compatible/scaleway/glm-test"

// gwCatalogMaxOutput is the operator table's max_output_tokens for the
// model — the budget the gateway serves, twice claw's 8192 default.
const gwCatalogMaxOutput = 16384

// gwWireRequest is the part of a chat-completions request body the fake
// gateway reads.
type gwWireRequest struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	MaxTokens       int             `json:"max_tokens"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Stream          bool            `json:"stream"`
}

func (r gwWireRequest) declares(name string) bool {
	for _, t := range r.Tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

// forcedTool is the function a named tool_choice forces, "" otherwise.
func (r gwWireRequest) forcedTool() string {
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(r.ToolChoice, &named) == nil {
		return named.Function.Name
	}
	return ""
}

// requiresTool reports tool_choice "required" — claw's first agentic turn.
func (r gwWireRequest) requiresTool() bool {
	var s string
	return json.Unmarshal(r.ToolChoice, &s) == nil && s == "required"
}

// kind names the turn the request is: the loop's forced first tool call, the
// loop's free turn, the forced structured_output call, or a structured call
// that declares the tool without forcing it.
func (r gwWireRequest) kind() string {
	switch {
	case r.requiresTool():
		return "loop-first"
	case r.declares("lookup"):
		return "loop"
	case r.forcedTool() == "structured_output":
		return "forced"
	case r.declares("structured_output"):
		return "unforced"
	default:
		return "bare"
	}
}

// gwReply is one streamed answer: reasoning (reasoning_content deltas),
// visible content, at most one tool call, the finish reason and the
// completion token count the usage chunk reports.
type gwReply struct {
	reasoning        string
	content          string
	toolName         string
	toolArgs         string
	finish           string
	completionTokens int
}

// cutAtBudget is a turn that spends the request's whole output budget on
// reasoning and never reaches an answer — the measured failure.
func cutAtBudget(r gwWireRequest) gwReply {
	return gwReply{
		reasoning:        "Let me weigh criterion 1.1 against the evidence once more, then every other one...",
		finish:           "length",
		completionTokens: r.MaxTokens,
	}
}

type fakeGateway struct {
	t     *testing.T
	srv   *httptest.Server
	reply func(r gwWireRequest) gwReply

	mu   sync.Mutex
	reqs []gwWireRequest
	// window, when set, refuses a request like vLLM does once its prompt
	// plus max_tokens exceeds it.
	window int
}

func newFakeGateway(t *testing.T, reply func(r gwWireRequest) gwReply) *fakeGateway {
	t.Helper()
	g := &fakeGateway{t: t, reply: reply}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) requests() []gwWireRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gwWireRequest(nil), g.reqs...)
}

func (g *fakeGateway) kinds() []string {
	var out []string
	for _, r := range g.requests() {
		out = append(out, r.kind())
	}
	return out
}

func (g *fakeGateway) serve(w http.ResponseWriter, req *http.Request) {
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		http.NotFound(w, req)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		g.t.Errorf("fake gateway: read body: %v", err)
		return
	}
	var r gwWireRequest
	if err := json.Unmarshal(body, &r); err != nil {
		g.t.Errorf("fake gateway: decode body: %v (%s)", err, body)
		return
	}
	g.mu.Lock()
	g.reqs = append(g.reqs, r)
	window := g.window
	g.mu.Unlock()
	// vLLM's rule: the prompt (here its bytes at four a token) plus
	// max_tokens must fit the model's window, or the request is refused.
	if prompt := len(body) / 4; window > 0 && prompt+r.MaxTokens > window {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"object":"error","message":"This model's maximum context length is %d tokens. However, you requested %d tokens (%d in the messages, %d in the completion).","type":"BadRequestError","code":400}`,
			window, prompt+r.MaxTokens, prompt, r.MaxTokens)
		return
	}

	var rep gwReply
	if r.kind() == "loop-first" {
		rep = gwReply{toolName: "lookup", toolArgs: `{"q":"criteria"}`, finish: "tool_calls", completionTokens: 20}
	} else {
		rep = g.reply(r)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	var buf bytes.Buffer
	frame := func(v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			g.t.Errorf("fake gateway: encode frame: %v", err)
			return
		}
		fmt.Fprintf(&buf, "data: %s\n\n", raw)
	}
	delta := func(d map[string]any) {
		frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": d}}})
	}
	// The reasoning streams in pieces, like a real gateway's deltas.
	for _, piece := range splitInThree(rep.reasoning) {
		delta(map[string]any{"role": "assistant", "content": "", "reasoning_content": piece})
	}
	if rep.content != "" {
		delta(map[string]any{"content": rep.content})
	}
	if rep.toolName != "" {
		delta(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": rep.toolName, "arguments": rep.toolArgs},
		}}})
	}
	frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": rep.finish}}})
	frame(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": rep.completionTokens}})
	buf.WriteString("data: [DONE]\n\n")
	_, _ = w.Write(buf.Bytes())
}

func splitInThree(s string) []string {
	if s == "" {
		return nil
	}
	a, b := len(s)/3, 2*len(s)/3
	return []string{s[:a], s[a:b], s[b:]}
}

// gwAdjudicateSchema is rgaa-audit's adjudicate_output, `verdicts: json`
// included.
func gwAdjudicateSchema(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := SchemaToJSON(&ir.Schema{Name: "adjudicate_output", Fields: []*ir.SchemaField{
		{Name: "batch_id", Type: ir.FieldTypeString},
		{Name: "verdicts", Type: ir.FieldTypeJSON},
		{Name: "notes", Type: ir.FieldTypeString},
	}})
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return raw
}

// runGatewayAdjudicate executes one tools+schema node on the gateway route
// against g, the way rgaa-audit's adjudicate runs on claw: one read tool,
// effort high, no max_tokens on the node.
func runGatewayAdjudicate(t *testing.T, g *fakeGateway) (delegate.Result, error) {
	t.Helper()
	return runGatewayAdjudicateWithMaxTokens(t, g, 0)
}

func runGatewayAdjudicateWithMaxTokens(t *testing.T, g *fakeGateway, maxTokens int) (delegate.Result, error) {
	t.Helper()
	return runGatewayAdjudicateWith(t, g, maxTokens, "Adjudicate batch b1.")
}

func runGatewayAdjudicateWith(t *testing.T, g *fakeGateway, maxTokens int, prompt string) (delegate.Result, error) {
	t.Helper()
	clearGatewayEnv(t)
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", g.srv.URL)
	// claw's OpenAI client refuses an empty key; the fake gateway ignores it.
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-fake-gateway")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("OPENAI_COMPATIBLE_MODELS", fmt.Sprintf(`{"scaleway/glm-test": {"context_window": 131072, "max_output_tokens": %d}}`, gwCatalogMaxOutput))
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)

	backend := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	return backend.Execute(context.Background(), delegate.Task{
		NodeID:          "adjudicate",
		Model:           gwModelSpec,
		UserPrompt:      prompt,
		OutputSchema:    gwAdjudicateSchema(t),
		HasTools:        true,
		ToolMaxSteps:    5,
		MaxTokens:       maxTokens,
		ReasoningEffort: "high",
		ToolDefs: []delegate.ToolDef{{
			Name:        "lookup",
			Description: "reads the evidence",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			Execute: func(context.Context, json.RawMessage) (string, error) {
				return "criterion 1.1: every image has a text alternative", nil
			},
		}},
	})
}

// assertVerdict fails unless res carries the model's verdict as the node's
// structured output.
func assertVerdict(t *testing.T, res delegate.Result, err error, g *fakeGateway) {
	t.Helper()
	if err != nil {
		t.Fatalf("the lot was lost: %v (turns: %v)", err, g.kinds())
	}
	if res.ParseFallback {
		t.Fatalf("the node fell back to the text wrapper %v instead of its verdict (turns: %v)", res.Output, g.kinds())
	}
	if res.Output["batch_id"] != "b1" || res.Output["notes"] != "ok" {
		t.Fatalf("Output = %v, want the model's verdict (turns: %v)", res.Output, g.kinds())
	}
	if v, ok := res.Output["verdicts"].([]any); !ok || len(v) != 1 {
		t.Fatalf("verdicts = %#v, want the one verdict the model gave", res.Output["verdicts"])
	}
}

// Case A — the gateway ignores the forced tool_choice and the model answers
// in reasoning_content, leaving content empty, on every turn. claw's chat
// wire drops reasoning_content, so the answer never reached iterion. The
// node must read it off the final turn's reasoning — for free: no recovery
// call at all.
func TestGatewayStructuredOutput_AnswerInReasoningContent(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		return gwReply{reasoning: "Evidence read, ruling now.\n" + gwVerdict, finish: "stop", completionTokens: 300}
	})
	res, err := runGatewayAdjudicate(t, g)
	assertVerdict(t, res, err, g)
	if got := g.kinds(); len(got) != 2 {
		t.Errorf("turns = %v, want the tool step and the final turn only — the answer was already there", got)
	}
}

// Case B — the model answers with plain-text JSON when the tool is not
// forced, and with nothing when it is. The final turn of the loop came back
// empty, the forced recovery too: one unforced re-ask must get the JSON.
func TestGatewayStructuredOutput_TextOnlyWhenNotForced(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		if r.kind() == "unforced" {
			return gwReply{content: gwVerdict, finish: "stop", completionTokens: 120}
		}
		return gwReply{finish: "stop", completionTokens: 3}
	})
	res, err := runGatewayAdjudicate(t, g)
	assertVerdict(t, res, err, g)
	if got := strings.Join(g.kinds(), ","); got != "loop-first,loop,forced,unforced" {
		t.Errorf("turns = %s, want the forced recovery then ONE unforced re-ask", got)
	}
}

// Case B, prose variant — the final turn narrates instead of answering, the
// forced recovery is empty: the node used to fall back to the text wrapper.
func TestGatewayStructuredOutput_ProseThenTextWhenNotForced(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		switch r.kind() {
		case "unforced":
			return gwReply{content: gwVerdict, finish: "stop", completionTokens: 120}
		case "loop":
			return gwReply{content: "Evidence gathered; I am ready to rule.", finish: "stop", completionTokens: 12}
		}
		return gwReply{finish: "stop", completionTokens: 3}
	})
	res, err := runGatewayAdjudicate(t, g)
	assertVerdict(t, res, err, g)
}

// Case C — the model calls the structured_output tool only when it is NOT
// forced: the unforced re-ask declares the tool and lets the model choose.
func TestGatewayStructuredOutput_ToolCallOnlyWhenNotForced(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		if r.kind() == "unforced" {
			return gwReply{toolName: "structured_output", toolArgs: gwVerdict, finish: "tool_calls", completionTokens: 120}
		}
		return gwReply{finish: "stop", completionTokens: 3}
	})
	res, err := runGatewayAdjudicate(t, g)
	assertVerdict(t, res, err, g)
}

// Case D, the measured one (runs 01a15f83 / 01a15f95: final turn and
// recovery both out=8192, text empty) — the model's reasoning outgrows
// claw's 8192 default budget, and the cut used to read as a clean empty
// stop. The gateway serves 16384 output tokens for this model
// (OPENAI_COMPATIBLE_MODELS), and with that budget the model finishes:
// every request of the node must carry it.
func TestGatewayStructuredOutput_BudgetFromTheCatalog(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		if r.MaxTokens < 12000 {
			return cutAtBudget(r)
		}
		return gwReply{reasoning: "Long deliberation over every criterion.", content: gwVerdict, finish: "stop", completionTokens: 11000}
	})
	res, err := runGatewayAdjudicate(t, g)
	assertVerdict(t, res, err, g)
	for i, r := range g.requests() {
		if r.MaxTokens != gwCatalogMaxOutput {
			t.Errorf("request %d (%s) max_tokens = %d, want the gateway's %d", i, r.kind(), r.MaxTokens, gwCatalogMaxOutput)
		}
	}
}

// Case D, past the ceiling — at effort high the reasoning outgrows even the
// gateway's budget; at a lower effort the model answers. The recovery must
// change something instead of repeating the request that was just cut: it
// runs at reasoning_effort low.
func TestGatewayStructuredOutput_RecoveryLowersTheEffortAfterACut(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		if r.ReasoningEffort == "high" {
			return cutAtBudget(r)
		}
		if r.kind() == "forced" {
			return gwReply{toolName: "structured_output", toolArgs: gwVerdict, finish: "tool_calls", completionTokens: 400}
		}
		return gwReply{content: gwVerdict, finish: "stop", completionTokens: 400}
	})
	res, err := runGatewayAdjudicate(t, g)
	assertVerdict(t, res, err, g)
	reqs := g.requests()
	if len(reqs) != 3 {
		t.Fatalf("turns = %v, want the tool step, the cut final turn and ONE recovery", g.kinds())
	}
	if rec := reqs[2]; rec.kind() != "forced" || rec.ReasoningEffort != "low" {
		t.Errorf("recovery = %s at effort %q, want the forced structured call at effort \"low\"", rec.kind(), rec.ReasoningEffort)
	}
}

// Case D, unrecoverable — every turn is cut whatever the effort. The node
// fails, but it names the cut, the budget and the remedy, and it does not
// buy an unforced re-ask that could only be cut the same way.
func TestGatewayStructuredOutput_ACutNamesTheBudget(t *testing.T) {
	g := newFakeGateway(t, cutAtBudget)
	_, err := runGatewayAdjudicate(t, g)
	if err == nil {
		t.Fatalf("a node whose every turn was cut succeeded (turns: %v)", g.kinds())
	}
	msg := err.Error()
	t.Logf("the node's failure: %s", msg)
	for _, want := range []string{"max_tokens", fmt.Sprint(gwCatalogMaxOutput), "reasoning_effort"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
	if got := strings.Join(g.kinds(), ","); got != "loop-first,loop,forced" {
		t.Errorf("turns = %s, want no re-ask after the forced recovery was cut at the lowest effort", got)
	}
}

// The healthy path stays one free turn: a model that answers in content
// costs no recovery.
func TestGatewayStructuredOutput_HealthyAnswerCostsNoRecovery(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		return gwReply{content: gwVerdict, finish: "stop", completionTokens: 300}
	})
	res, err := runGatewayAdjudicate(t, g)
	assertVerdict(t, res, err, g)
	if got := strings.Join(g.kinds(), ","); got != "loop-first,loop" {
		t.Errorf("turns = %s, want the tool step and the final turn", got)
	}
}

// The catalog's budget is a default: a node that sets max_tokens keeps it,
// on every request.
func TestGatewayStructuredOutput_TheNodesMaxTokensWins(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		return gwReply{content: gwVerdict, finish: "stop", completionTokens: 300}
	})
	res, err := runGatewayAdjudicateWithMaxTokens(t, g, 12000)
	assertVerdict(t, res, err, g)
	for i, r := range g.requests() {
		if r.MaxTokens != 12000 {
			t.Errorf("request %d (%s) max_tokens = %d, want the node's 12000", i, r.kind(), r.MaxTokens)
		}
	}
}

// A reasoning that ends on a draft does not outrank the model's visible
// ruling: the final turn shows text, so the recovery pass rules.
func TestGatewayStructuredOutput_AReasoningDraftNeverOutranksTheText(t *testing.T) {
	const ruled = `{"batch_id":"b1","verdicts":[{"id":"1.1","verdict":"NC"}],"notes":"img alt missing on /home"}`
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		if r.kind() == "forced" {
			return gwReply{toolName: "structured_output", toolArgs: ruled, finish: "tool_calls", completionTokens: 200}
		}
		return gwReply{
			reasoning:        "First impression, draft:\n" + gwVerdict,
			content:          "Checking the evidence again: the home page image has no text alternative, so 1.1 is NOT compliant.",
			finish:           "stop",
			completionTokens: 400,
		}
	})
	res, err := runGatewayAdjudicate(t, g)
	if err != nil {
		t.Fatalf("the lot was lost: %v (turns: %v)", err, g.kinds())
	}
	if got := strings.Join(g.kinds(), ","); got != "loop-first,loop,forced" {
		t.Errorf("turns = %s, want the recovery pass to rule", got)
	}
	if v, _ := res.Output["verdicts"].([]any); len(v) != 1 || v[0].(map[string]any)["verdict"] != "NC" {
		t.Errorf("verdicts = %v, want the recovery's NC — the draft's C was taken", res.Output["verdicts"])
	}
}

// The gateway budget is fitted to the window per request: a prompt that
// leaves less room than the catalog's max output is sent at claw's own
// 8192, which a vLLM-style window accepts — the request does not fail
// where it used to pass.
func TestGatewayStructuredOutput_TheBudgetFitsTheWindow(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		return gwReply{content: gwVerdict, finish: "stop", completionTokens: 300}
	})
	g.window = 131072
	// ~118k tokens at the gateway's four bytes a token.
	prompt := "Adjudicate batch b1 against this evidence:\n" + strings.Repeat("The image on the home page has an alt attribute. ", 9600)
	res, err := runGatewayAdjudicateWith(t, g, 0, prompt)
	assertVerdict(t, res, err, g)
	for i, r := range g.requests() {
		if r.MaxTokens != defaultMaxTokens {
			t.Errorf("request %d (%s) max_tokens = %d, want claw's %d for a prompt that fills the window", i, r.kind(), r.MaxTokens, defaultMaxTokens)
		}
	}
}

// A node with an output schema and no tools, served by the gateway: a model
// that answers its forced structured call in reasoning_content is read on
// that one call.
func TestGatewayStructuredOutput_ANodeWithoutToolsAnswersInReasoning(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		return gwReply{reasoning: "Ruling on the given evidence.\n" + gwVerdict, finish: "stop", completionTokens: 300}
	})
	clearGatewayEnv(t)
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", g.srv.URL)
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-fake-gateway")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("OPENAI_COMPATIBLE_MODELS", "")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
	backend := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	res, err := backend.Execute(context.Background(), delegate.Task{
		NodeID: "adjudicate", Model: gwModelSpec, UserPrompt: "Adjudicate batch b1 from the evidence above.",
		OutputSchema: gwAdjudicateSchema(t), ReasoningEffort: "high",
	})
	assertVerdict(t, res, err, g)
	if got := strings.Join(g.kinds(), ","); got != "forced" {
		t.Errorf("turns = %s, want the one forced call", got)
	}
}

// The recovery pass of a tools+schema node reads a reasoning answer too:
// the final turn ended with prose reasoning and no text, the forced
// recovery answered in reasoning_content.
func TestGatewayStructuredOutput_TheRecoveryAnswersInReasoning(t *testing.T) {
	g := newFakeGateway(t, func(r gwWireRequest) gwReply {
		if r.kind() == "forced" {
			return gwReply{reasoning: "Formatting the ruling.\n" + gwVerdict, finish: "stop", completionTokens: 300}
		}
		return gwReply{reasoning: "I have read everything I need.", finish: "stop", completionTokens: 40}
	})
	res, err := runGatewayAdjudicate(t, g)
	assertVerdict(t, res, err, g)
	if got := strings.Join(g.kinds(), ","); got != "loop-first,loop,forced" {
		t.Errorf("turns = %s, want the recovery pass to answer", got)
	}
}
