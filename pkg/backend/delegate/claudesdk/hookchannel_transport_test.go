package claudesdk

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A bound that travels on ARGV follows the task; a bound that travels on a
// HOOK follows the call site. These tests read the wire of one spawn — the
// argv it was given and the lines written to its stdin — because that is the
// only place the difference shows. Asserting on config would prove the
// intention, not the transport: the defect #1672 measured was precisely a
// config whose hooks were dropped on the way to the process.

// fakeCLI builds a CommandBuilder whose child records its argv and its stdin,
// then answers with the NDJSON lines the caller asked for. The recordings are
// files so the assertions read what the CHILD saw, not what the parent meant.
type fakeCLI struct {
	t       *testing.T
	dir     string
	replies []string
}

func newFakeCLI(t *testing.T, replies ...string) *fakeCLI {
	t.Helper()
	return &fakeCLI{t: t, dir: t.TempDir(), replies: replies}
}

func (f *fakeCLI) builder() CommandBuilder {
	return func(ctx context.Context, cliPath string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
		var out strings.Builder
		for _, r := range f.replies {
			out.WriteString(r)
			out.WriteString("\n")
		}
		// Replies go out FIRST so the parent can process them (and answer a
		// hook callback) while this child is still reading. The stdin capture
		// then reads to EOF, which is the REAL contract of a stream-json CLI:
		// it does not exit at the end of a turn, it waits for more input. So
		// this child only exits once the parent closes stdin — a parent that
		// waits for the child instead is a circular wait, and this fixture is
		// what makes that hang a test failure rather than a production stall.
		// A time-bounded read here would hide exactly that defect.
		script := `printf '%s\n' "$@" > "$REC_DIR/argv"; cat "$REC_DIR/replies"; cat > "$REC_DIR/stdin"`
		if err := os.WriteFile(filepath.Join(f.dir, "replies"), []byte(out.String()), 0o644); err != nil {
			f.t.Errorf("seeding the fake CLI's replies: %v", err)
			return nil
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", script, "fake-claude")
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(), "REC_DIR="+f.dir)
		return cmd
	}
}

func (f *fakeCLI) argv(t *testing.T) []string {
	t.Helper()
	return f.lines(t, "argv")
}

func (f *fakeCLI) stdin(t *testing.T) []string {
	t.Helper()
	return f.lines(t, "stdin")
}

func (f *fakeCLI) lines(t *testing.T, name string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading the recorded %s: %v", name, err)
	}
	var out []string
	for _, l := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

const fakeResult = `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`

func denyBash(t *testing.T) HookMatcher {
	t.Helper()
	return HookMatcher{Handler: func(ctx context.Context, in HookCallbackInput) (HookOutput, error) {
		return HookOutput{Decision: "deny", DecisionReason: "Bash is denied for this task"}, nil
	}}
}

// The witness. A one-shot spawn carrying hooks must put the `initialize`
// control request on its stdin — before the prompt — or the gate the task
// declared does not reach the process that runs it. At HEAD before this
// change, stdin was 0 lines.
func TestAHookedOneShotSpawnSendsItsInitializeOnStdin(t *testing.T) {
	fake := newFakeCLI(t, fakeResult)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	if _, err := Prompt(ctx, "format this as JSON",
		WithCommandBuilder(fake.builder()),
		WithDisallowedTools("Bash"),
		WithHook(HookPreToolUse, denyBash(t)),
	); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// Termination is half the unit. A hooked spawn reads stdin as a protocol
	// stream, so the CLI waits for more input after the turn; if Prompt waited
	// for EOF in return, the pair would deadlock and only the context deadline
	// would break it — a gated structured-output pass would burn its node's
	// whole remaining budget and lose its output. The CLI stand-in above
	// exits only on stdin EOF, so a slow return here means Prompt stopped
	// terminating on its own.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the hooked spawn took %s to return — it is waiting for a CLI that is waiting for it", elapsed)
	}

	stdin := fake.stdin(t)
	if len(stdin) < 2 {
		t.Fatalf("the hooked spawn wrote %d stdin lines, want at least 2 (initialize, then the prompt) — its gate never reached the process:\n%v", len(stdin), stdin)
	}

	var req struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string                      `json:"subtype"`
			Hooks   map[string][]hookMatcherCfg `json:"hooks"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(stdin[0]), &req); err != nil {
		t.Fatalf("stdin line 1 is not a control request: %v\n%s", err, stdin[0])
	}
	if req.Type != "control_request" || req.Request.Subtype != "initialize" {
		t.Fatalf("stdin line 1 is %q/%q, want control_request/initialize:\n%s", req.Type, req.Request.Subtype, stdin[0])
	}
	if len(req.Request.Hooks["PreToolUse"]) != 1 {
		t.Fatalf("the initialize request carries %d PreToolUse matchers, want 1 — the hooks were dropped on the way to the process:\n%s",
			len(req.Request.Hooks["PreToolUse"]), stdin[0])
	}
	if ids := req.Request.Hooks["PreToolUse"][0].HookCallbackIDs; len(ids) != 1 || ids[0] != "hook_PreToolUse_0" {
		t.Fatalf("the matcher registers callback ids %v, want [hook_PreToolUse_0] — a callback the CLI names must resolve to a handler", ids)
	}

	// The prompt travels on stdin now, because the CLI only reads stdin as a
	// protocol stream under --input-format stream-json.
	argv := strings.Join(fake.argv(t), " ")
	if !strings.Contains(argv, "--input-format stream-json") {
		t.Fatalf("a hooked spawn must ask the CLI to read stdin as a protocol stream, argv was:\n%s", argv)
	}
	if strings.Contains(argv, "format this as JSON") {
		t.Fatalf("the prompt is still in argv on a hooked spawn, so the CLI would never read the stdin the hooks are on:\n%s", argv)
	}
	var user struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(stdin[1]), &user); err != nil {
		t.Fatalf("stdin line 2 is not a user message: %v\n%s", err, stdin[1])
	}
	if user.Type != "user" || user.Message.Content != "format this as JSON" {
		t.Fatalf("stdin line 2 does not carry the prompt: %+v", user)
	}
	if !strings.Contains(argv, "--disallowedTools Bash") {
		t.Fatalf("the argv bound must survive the switch to stdin, argv was:\n%s", argv)
	}
}

// hookMatcherCfg mirrors the wire shape of hookMatcherConfig for the assertion
// above. It is declared here rather than reused so that a change to the sent
// shape has to be made twice, deliberately — the test is the reader the CLI
// stands in for.
type hookMatcherCfg struct {
	Matcher         *string  `json:"matcher"`
	HookCallbackIDs []string `json:"hookCallbackIds"`
	Timeout         *float64 `json:"timeout"`
}

// Registering hooks and then refusing their callbacks is not a slower gate,
// it is no gate: the CLI asks, gets an error, and the default decision is the
// CLI's, not the task's. Before this change the one-shot path answered
// anything but can_use_tool with "unsupported request".
func TestAHookedOneShotSpawnAnswersItsHookCallbacks(t *testing.T) {
	callback := `{"type":"control_request","request_id":"r1","request":{"subtype":"hook_callback","callback_id":"hook_PreToolUse_0","input":{"hook_event_name":"PreToolUse","tool_name":"Bash"}}}`
	fake := newFakeCLI(t, callback, fakeResult)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	called := false
	matcher := HookMatcher{Handler: func(ctx context.Context, in HookCallbackInput) (HookOutput, error) {
		called = true
		if in.ToolName != "Bash" {
			t.Errorf("the handler was asked about %q, want Bash", in.ToolName)
		}
		return HookOutput{Decision: "deny", DecisionReason: "Bash is denied for this task"}, nil
	}}

	if _, err := Prompt(ctx, "format this as JSON",
		WithCommandBuilder(fake.builder()),
		WithHook(HookPreToolUse, matcher),
	); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !called {
		t.Fatal("the CLI asked the hook and no handler ran — the one-shot path still refuses hook callbacks")
	}

	// The verdict has to reach the CLI, in the shape it validates.
	var answered bool
	for _, line := range fake.stdin(t) {
		var resp struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Response  struct {
					HookSpecificOutput struct {
						HookEventName            string `json:"hookEventName"`
						PermissionDecision       string `json:"permissionDecision"`
						PermissionDecisionReason string `json:"permissionDecisionReason"`
					} `json:"hookSpecificOutput"`
				} `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil || resp.Type != "control_response" {
			continue
		}
		if resp.Response.RequestID != "r1" {
			t.Fatalf("the response answers request %q, want r1", resp.Response.RequestID)
		}
		if resp.Response.Subtype != "success" {
			t.Fatalf("the hook verdict came back as %q, not success: %s", resp.Response.Subtype, line)
		}
		hso := resp.Response.Response.HookSpecificOutput
		if hso.PermissionDecision != "deny" || hso.HookEventName != "PreToolUse" {
			t.Fatalf("the CLI was told %q for %q, want deny/PreToolUse: %s", hso.PermissionDecision, hso.HookEventName, line)
		}
		if hso.PermissionDecisionReason == "" {
			t.Fatalf("a deny with no reason reads as an unexplained refusal: %s", line)
		}
		answered = true
	}
	if !answered {
		t.Fatalf("no control_response reached the CLI's stdin — the handler ran and its verdict was dropped:\n%v", fake.stdin(t))
	}
}

// The unhooked one-shot spawn must not change: the prompt stays in argv and
// stdin stays closed, or every ungated structured-output pass pays for a
// stdin protocol it does not need (and the CLI warns about a silent stdin).
func TestAnUnhookedOneShotSpawnKeepsThePromptInArgv(t *testing.T) {
	fake := newFakeCLI(t, fakeResult)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := Prompt(ctx, "format this as JSON", WithCommandBuilder(fake.builder())); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	argv := fake.argv(t)
	if len(argv) == 0 || argv[len(argv)-1] != "format this as JSON" {
		t.Fatalf("the unhooked spawn must carry the prompt as its last argument, argv was:\n%v", argv)
	}
	if joined := strings.Join(argv, " "); strings.Contains(joined, "--input-format") {
		t.Fatalf("the unhooked spawn asked for a stdin protocol it never writes to:\n%s", joined)
	}
	if stdin := fake.stdin(t); len(stdin) != 0 {
		t.Fatalf("the unhooked spawn wrote %d stdin lines, want 0:\n%v", len(stdin), stdin)
	}
}

// Both spawn paths register through the same channel, so a callback id minted
// by one resolves in the other. This is the property that makes the channel a
// chokepoint rather than a second implementation.
func TestBothSpawnPathsMintTheSameCallbackIDs(t *testing.T) {
	hooks := map[HookEvent][]HookMatcher{
		HookPreToolUse: {denyBash(t), denyBash(t)},
		HookStop:       {denyBash(t)},
	}
	a, b := newHookChannel(), newHookChannel()
	bodyA, bodyB := a.initializeBody(hooks), b.initializeBody(hooks)

	jsonA, err := json.Marshal(bodyA)
	if err != nil {
		t.Fatal(err)
	}
	jsonB, err := json.Marshal(bodyB)
	if err != nil {
		t.Fatal(err)
	}
	if string(jsonA) != string(jsonB) {
		t.Fatalf("the same hooks produced two different initialize bodies:\n%s\n%s", jsonA, jsonB)
	}
	for _, want := range []string{"hook_PreToolUse_0", "hook_PreToolUse_1", "hook_Stop_0"} {
		if _, ok := a.callbacks[want]; !ok {
			t.Errorf("callback id %s was sent to the CLI but resolves to no handler", want)
		}
	}
	if len(a.callbacks) != 3 {
		t.Errorf("registered %d handlers for 3 matchers", len(a.callbacks))
	}
}

// "The engine registered my hooks" and "the engine refused them" must not read
// alike. The initialize is sent without waiting, so nothing routes its answer:
// the error response is the only place a CLI that does not accept hooks on this
// path says so. A spawn that read past it would run the task's gate NOWHERE
// while every argv bound and every log line said it was gated — and the live
// proof this unit still owes is exactly a reading of this response.
func TestARefusedOneShotHookRegistrationFailsLoudly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The fake answers whatever request id it is given, so the refusal is
	// matched by id rather than by position.
	fake := newFakeCLI(t)
	fake.replies = []string{"__REFUSE__", fakeResult}

	_, err := Prompt(ctx, "format this as JSON",
		WithCommandBuilder(fake.refusingBuilder()),
		WithHook(HookPreToolUse, denyBash(t)),
	)
	if err == nil {
		t.Fatal("the CLI refused the hook registration and Prompt returned success — the spawn would run ungated while looking gated")
	}
	for _, want := range []string{"refused", "gate absent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must say what was lost; %q does not mention %q", err.Error(), want)
		}
	}
	if !strings.Contains(err.Error(), "hooks are not supported here") {
		t.Errorf("the error must carry the engine's own reason, got %q", err.Error())
	}
}

// A refusal carrying a DIFFERENT request id is not this spawn's refusal, and
// must not abort it: an unrelated control response is noise, not a verdict.
func TestAnUnrelatedControlResponseDoesNotAbortTheSpawn(t *testing.T) {
	stray := `{"type":"control_response","response":{"subtype":"error","request_id":"someone_else","error":"not about this spawn"}}`
	fake := newFakeCLI(t, stray, fakeResult)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := Prompt(ctx, "format this as JSON",
		WithCommandBuilder(fake.builder()),
		WithHook(HookPreToolUse, denyBash(t)),
	); err != nil {
		t.Fatalf("an unrelated control response aborted the spawn: %v", err)
	}
}

// refusingBuilder answers the initialize with an error response carrying the
// request id the parent actually minted, read back from the recorded stdin.
func (f *fakeCLI) refusingBuilder() CommandBuilder {
	return func(ctx context.Context, cliPath string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
		// Read the first stdin line, echo a refusal quoting its request_id,
		// then drain the rest. sed extracts the id without a JSON parser.
		script := `printf '%s\n' "$@" > "$REC_DIR/argv"
read -r first
printf '%s\n' "$first" > "$REC_DIR/stdin"
id=$(printf '%s' "$first" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"hooks are not supported here"}}\n' "$id"
cat >> "$REC_DIR/stdin"`
		cmd := exec.CommandContext(ctx, "sh", "-c", script, "fake-claude")
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(), "REC_DIR="+f.dir)
		return cmd
	}
}
