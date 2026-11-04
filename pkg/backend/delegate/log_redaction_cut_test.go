package delegate

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// The delegate backends quote third-party and model text into the run's log
// and into returned errors, and some of those quotes are CUT. A cut that
// lands inside a declared value leaves a head no matcher recognises, so the
// run's pass crosses the text BEFORE the cut, at every such site
// (iterion#2822). The guard below is the production pass — the same
// function runview.BuildExecutor installs on the run's logger and hands
// Task.RedactSecrets.

// redactingLogger builds the run's logger shape: the pass installed on the
// handle, records kept in buf.
func redactingLogger(t *testing.T) (*iterlog.Logger, *strings.Builder, string, string) {
	t.Helper()
	const value = "hunter2-9f8e7d6c5b4a-db-password"
	placeholder := secretguard.PlaceholderForName("db")
	guard := secretguard.New([]secretguard.Secret{
		{Name: "db", Value: value, Placeholder: placeholder},
	}, secretguard.DefaultConfig())
	var buf strings.Builder
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	logger.SetRedactor(guard.Redact)
	return logger, &buf, value, placeholder
}

// The pi backend's steer line rides the run's logger and CUTS the steer
// text at 200 bytes. The steer is model-derived text (the same text
// pkg/supervise logs and forwards), so a value straddling the cut must
// never publish its head.
func TestThePiSteerLineRedactsBeforeItCuts(t *testing.T) {
	logger, _, value, placeholder := redactingLogger(t)
	b := &PiRPCBackend{Logger: logger}

	// The steer straddles the 200-byte cut: the value starts at byte 190,
	// so a cut taken first keeps exactly the head the cut leaves whole.
	steer := strings.Repeat("re-read the diff. ", 11)[:190] + value + " — then rerun"
	at := strings.Index(steer, value)
	if len(steer) <= 200 || at < 0 || at >= 200 || at+len(value) <= 200 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the cut (len=%d, at=%d)", len(steer), at)
	}
	head := value[:200-at]

	got := b.steerLogLine(steer)
	if !strings.Contains(got, placeholder[:10]) {
		t.Errorf("the logged steer shows no placeholder where the value was: %q", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("the cut published the value's head %q — the line cut before the pass: %q", head, got)
	}
	if !strings.Contains(got, "...") {
		t.Errorf("a 200+ byte steer must come back bounded: %q", got)
	}
}

// A third-party extension's dialog is declined with its prompt quoted into
// the run's log, cut at 120 bytes. Same discipline: pass before cut.
func TestTheUIDeclineLineRedactsBeforeItCuts(t *testing.T) {
	logger, buf, value, placeholder := redactingLogger(t)
	b := &PiRPCBackend{Logger: logger}
	collector := &piCollector{logger: logger, settled: make(chan struct{})}

	title := strings.Repeat("approve this file ", 7)[:110] + value + " tail"
	req := pisdk.UIRequest{Type: "extension_ui_request", ID: "u1", Method: "vendor.dialog", Title: title}
	at := strings.Index(req.Prompt(), value)
	if at < 0 || at >= 120 || at+len(value) <= 120 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the cut (at=%d)", at)
	}
	head := value[:120-at]

	if resp := b.handleUIRequest(Task{NodeID: "n1", Iteration: 1}, collector, req); resp != nil {
		t.Fatalf("a third-party dialog must be cancelled with no reply, got %v", resp)
	}

	got := buf.String()
	if !strings.Contains(got, "declining an unrecognised extension UI request") {
		t.Fatalf("the decline line never reached the log, so this test proves nothing: %q", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("the cut published the value's head %q — the line cut before the pass: %q", head, got)
	}
	if !strings.Contains(got, placeholder[:10]) {
		t.Errorf("the decline line shows no placeholder where the value was: %q", got)
	}
}

// An ask_user question is process-derived text quoted into the run's log,
// cut at 200 bytes. Same discipline: pass before cut.
func TestTheAskUserLineRedactsBeforeItCuts(t *testing.T) {
	logger, buf, value, placeholder := redactingLogger(t)
	b := &PiRPCBackend{Logger: logger}
	collector := &piCollector{logger: logger, settled: make(chan struct{})}

	question := strings.Repeat("ship it? ", 22)[:190] + value + " tail"
	raw := `{"__iterion":1,"op":"ask_user","data":{"question":"` + question + `"}}`
	req := pisdk.UIRequest{Type: "extension_ui_request", ID: "u2", Method: "ctx.ui.input", Message: raw}
	env, ok := piParseCtrl(req)
	if !ok || env.Op != piOpAskUser {
		t.Fatalf("the premise of this test is wrong: the envelope must parse as ask_user, got %q", raw)
	}
	at := strings.Index(question, value)
	if at < 0 || at >= 200 || at+len(value) <= 200 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the cut (at=%d)", at)
	}
	head := value[:200-at]

	if resp := b.handleUIRequest(Task{NodeID: "n1", Iteration: 1}, collector, req); resp == nil {
		t.Fatal("an ask_user envelope must be answered")
	}

	if p := collector.pending(); p == nil || p.question != question {
		t.Fatalf("the ask_user pause never landed, so this test proves nothing: %v", p)
	}

	got := buf.String()
	if !strings.Contains(got, "ask_user:") {
		t.Fatalf("the ask_user line never reached the log, so this test proves nothing: %q", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("the cut published the value's head %q — the line cut before the pass: %q", head, got)
	}
	if !strings.Contains(got, placeholder[:10]) {
		t.Errorf("the ask_user line shows no placeholder where the value was: %q", got)
	}
}

// The codex tool-use line's header formats the model's input through
// toolUseDetail, which keeps 100 bytes: a declared value straddling the cut
// published its head while the body below showed the placeholder. The pass
// crosses every leaf BEFORE the display cut, on the decoded strings
// (RedactLeaves), exactly as the claude_code stream renders its own headers
// (iterion#2822).
func TestTheCodexToolUseHeaderRedactsBeforeItCuts(t *testing.T) {
	logger, buf, value, placeholder := redactingLogger(t)
	b := &CodexBackend{Logger: logger}

	cmd := `curl -s -X POST https://internal.example.com/api/v2/deploy -H "Authorization: Bearer ` + value + `" https://api.example.com/v2/ingest`
	at := strings.Index(cmd, value)
	if at < 0 || at >= 100 || at+len(value) <= 100 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the 100-byte header cut (at=%d, len=%d)", at, len(cmd))
	}
	// HeaderDetail's truncate keeps n-3 bytes before its "..." suffix.
	head := value[:min(100-3-at, len(value))]

	msg := &codexsdk.AssistantMessage{Content: []codexsdk.ContentBlock{
		&codexsdk.ToolUseBlock{Name: "Bash", Input: map[string]any{"command": cmd}},
	}}
	b.logAssistantActivity("n1", 1, msg, func(s string) string {
		return strings.ReplaceAll(s, value, placeholder)
	})

	got := buf.String()
	if !strings.Contains(got, "[n1#1/codex] 🔧 Bash") {
		t.Fatalf("the tool-use line never reached the log, so this test proves nothing: %q", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("the tool-use header published the value's head %q — the display cut ran before the pass: %q", head, got)
	}
	if !strings.Contains(got, placeholder) {
		t.Errorf("the tool-use line shows no placeholder where the value was: %q", got)
	}
}

// The steer log line is a CALL SITE property, not only the helper's: the
// pi backend forwards an operator steer and logs it in drainInbox. A
// refactor that inlines the cut there — truncate(msg, 200) without the
// pass — leaves steerLogLine correct and unreferenced while the leak
// returns. The sweep pins the site: every log call quoting the steer must
// route through the redacting helper (or carry Logger.Redact itself).
func TestTheSteerLogSiteRoutesThroughTheRedactingHelper(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || !strings.Contains(lit.Value, "steered: %s") {
				return true
			}
			seen++
			for _, arg := range call.Args[1:] {
				text := exprText(t, fset, arg)
				if strings.Contains(text, "steerLogLine") || strings.Contains(text, "Logger.Redact") || strings.Contains(text, ".Redact(") {
					return true
				}
			}
			t.Errorf("%s logs the steer text without the run's pass — it must route through steerLogLine (or carry Logger.Redact before its truncate): %s",
				name, exprText(t, fset, call))
			return true
		})
	}
	if seen == 0 {
		t.Fatalf("the sweep found no log site quoting the steer — the predicate stopped matching")
	}
}

// exprText prints an AST expression back to source.
func exprText(t *testing.T, fset *token.FileSet, e ast.Node) string {
	t.Helper()
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, e); err != nil {
		t.Fatalf("print: %v", err)
	}
	return buf.String()
}

// The codex terminal error quotes result text, cut at 500 bytes, and the
// error is then logged and recorded whole — where the run's pass applies to
// the FULL text but cannot recognise a value the cut already split. The
// pass runs at the quote, before the cut.
func TestTheCodexTerminalErrorRedactsBeforeItCuts(t *testing.T) {
	_, _, value, placeholder := redactingLogger(t)
	guard := secretguard.New([]secretguard.Secret{
		{Name: "db", Value: value, Placeholder: placeholder},
	}, secretguard.DefaultConfig())

	text := "Error: unexpected status 401 Unauthorized · " + strings.Repeat("x", 440) + value + " end"
	rm := &codexsdk.ResultMessage{Result: &text, IsError: true, Subtype: "error"}
	at := strings.Index(text, value)
	if at < 0 || at >= 500 || at+len(value) <= 500 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the cut (at=%d)", at)
	}
	head := value[:500-at]

	err := codexTerminalFailure(rm, "", guard.Redact)
	if err == nil {
		t.Fatal("codexTerminalFailure returned nil — the terminal-error branch never ran")
	}
	got := err.Error()
	if !strings.Contains(got, "codex terminal error") {
		t.Fatalf("unexpected error shape: %q", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("the cut published the value's head %q — the error cut before the pass: %q", head, got)
	}
	if !strings.Contains(got, placeholder[:10]) {
		t.Errorf("the error shows no placeholder where the value was: %q", got)
	}
}

// The codex tool-error content is model-emitted text flattened for the
// run's log and cut at 500 bytes. Same discipline: pass before cut.
func TestTheCodexToolErrorContentRedactsBeforeItCuts(t *testing.T) {
	_, _, value, placeholder := redactingLogger(t)

	blocks := []codexsdk.ContentBlock{
		&codexsdk.TextBlock{Text: strings.Repeat("tool failed: ", 38) + value + " end"},
	}
	at := strings.Index(blocks[0].(*codexsdk.TextBlock).Text, value)
	if at < 0 || at >= 500 || at+len(value) <= 500 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the cut (at=%d)", at)
	}
	head := value[:500-at]

	got := contentBlocksText(blocks, func(s string) string {
		return strings.ReplaceAll(s, value, placeholder)
	})
	// The placeholder starts where the value did, inside the cut window, so
	// the cut keeps only its head — assert exactly that much of it.
	kept := 500 - at
	if kept > len(placeholder) {
		kept = len(placeholder)
	}
	if !strings.Contains(got, placeholder[:kept]) {
		t.Errorf("the flattened content shows no placeholder where the value was: %q", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("the cut published the value's head %q — the content cut before the pass: %q", head, got)
	}
}

// A malformed envelope line is quoted into the decode error, cut at 256
// bytes. The reader's pass (Multiplexer.SetRedactor, wired from
// Task.RedactSecrets in pkg/backend/model) crosses the line before the
// cut, or a value straddling it publishes its head into the node's error.
func TestTheEnvelopeDecodeErrorRedactsBeforeItCuts(t *testing.T) {
	_, _, value, placeholder := redactingLogger(t)

	line := `{"type":"tool_result","id":"x","data":{"output":"` + strings.Repeat("y", 247-len(`{"type":"tool_result","id":"x","data":{"output":"`)) + value + `"},,}`
	at := strings.Index(line, value)
	if at < 0 || at >= 256 || at+len(value) <= 256 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the cut (at=%d, len=%d)", at, len(line))
	}
	head := value[:256-at]

	er := NewEnvelopeReader(strings.NewReader(line))
	er.SetRedactor(func(s string) string { return strings.ReplaceAll(s, value, placeholder) })
	_, err := er.Read()
	if err == nil || !strings.Contains(err.Error(), "delegate: decode envelope") {
		t.Fatalf("the premise of this test is wrong: the line must fail to decode, got %v", err)
	}
	got := err.Error()
	if strings.Contains(got, head) {
		t.Errorf("the cut published the value's head %q — the error cut before the pass: %q", head, got)
	}
	if !strings.Contains(got, placeholder[:9]) {
		t.Errorf("the quoted line shows no placeholder where the value was: %q", got)
	}

	// A reader without a pass keeps the raw line: the pass is the
	// consumer's wiring, not the transport's invention.
	er2 := NewEnvelopeReader(strings.NewReader(line))
	_, err2 := er2.Read()
	if err2 == nil || !strings.Contains(err2.Error(), value[:9]) {
		t.Errorf("without a pass the raw line must be quoted as before: %v", err2)
	}
}
