package delegate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestSessionLedgerSettlesWhatWasToldAndRecordsWhatWasLeft(t *testing.T) {
	l := NewSessionLedger(map[string][]string{"s": {"a", "b"}})
	l.Settle("s", []string{"a"}, []string{"c"})
	if got := l.Terminated("s"); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("entry = %v, want [b c]: the told one leaves, the untold one stays, the new one joins", got)
	}
	l.Settle("s", []string{"b", "c"}, nil)
	if got := l.Terminated("s"); got != nil {
		t.Fatalf("entry = %v, want none once everything was told and nothing was left", got)
	}
	if got := l.Entries(); got != nil {
		t.Fatalf("entries = %v, want an empty entry dropped", got)
	}
}

// Two processes of one session told the same entry end in an order nobody
// can know: the entry they settle to must not depend on it.
func TestSessionLedgerSettleIsOrderIndependent(t *testing.T) {
	settle := func(first, second func(*MemorySessionLedger)) []string {
		l := NewSessionLedger(map[string][]string{"s": {"t9"}})
		first(l)
		second(l)
		return l.Terminated("s")
	}
	cleanEnd := func(l *MemorySessionLedger) { l.Settle("s", []string{"t9"}, nil) }
	leftWork := func(l *MemorySessionLedger) { l.Settle("s", []string{"t9"}, []string{"t10"}) }
	ab, ba := settle(cleanEnd, leftWork), settle(leftWork, cleanEnd)
	if !reflect.DeepEqual(ab, []string{"t10"}) || !reflect.DeepEqual(ba, []string{"t10"}) {
		t.Fatalf("clean-then-left = %v, left-then-clean = %v; want [t10] both ways", ab, ba)
	}
}

func TestSessionLedgerKeepsItsOwnCopies(t *testing.T) {
	in := map[string][]string{"s": {"a"}, "": {"x"}, "empty": nil}
	l := NewSessionLedger(in)
	in["s"][0] = "mutated"
	got := l.Terminated("s")
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("entry = %v, want the ledger's own copy", got)
	}
	got[0] = "mutated too"
	if again := l.Terminated("s"); !reflect.DeepEqual(again, []string{"a"}) {
		t.Fatalf("entry = %v after mutating a returned slice", again)
	}
	if e := l.Entries(); len(e) != 1 {
		t.Fatalf("entries = %v, want the empty id and the empty entry dropped", e)
	}
}

func TestANilSessionLedgerRecordsNothing(t *testing.T) {
	var l *MemorySessionLedger
	l.Settle("s", nil, []string{"x"})
	if l.Terminated("s") != nil || l.Entries() != nil {
		t.Fatal("a nil ledger answered something")
	}
	var none SessionLedger
	settleLedger(none, "s", nil, []string{"x"})
	if ledgerTerminated(none, "s") != nil {
		t.Fatal("no ledger answered something")
	}
}

func TestTheTerminatedBackgroundNotePrefixesThePrompt(t *testing.T) {
	got := terminatedBackgroundNote([]string{"auditor (local_agent, t1)"}, "do the work")
	if !strings.HasPrefix(got, "[iterion] This session resumes in a new process") ||
		!strings.Contains(got, "auditor (local_agent, t1)") || !strings.HasSuffix(got, "do the work") {
		t.Fatalf("prompt = %q, want the note before the prompt", got)
	}
	if got := terminatedBackgroundNote(nil, "p"); got != "p" {
		t.Fatalf("prompt = %q, want it untouched when nothing was told", got)
	}
}

// fakeClaudeStdin logs what the SDK writes on stdin up to the prompt, then
// answers the minimum stream-json that ends a turn, in the session
// $FAKE_SESSION_ID (s1 by default).
const fakeClaudeStdin = `#!/bin/sh
sid="${FAKE_SESSION_ID:-s1}"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$STDIN_LOG"
  case "$line" in *'"type":"user"'*) break ;; esac
done
printf '{"type":"system","subtype":"init","session_id":"%s","model":"fake","tools":[],"mcp_servers":[]}\n' "$sid"
printf '{"type":"result","subtype":"success","is_error":false,"result":"ok","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"%s"}\n' "$sid"
`

type ledgerRun struct {
	stdin  string
	ledger *MemorySessionLedger
}

// runLedgerExecute drives Execute against the stand-in CLI with a ledger
// seeded with entries, and returns what the CLI read and the ledger after.
func runLedgerExecute(t *testing.T, task Task, entries map[string][]string, reportedSession string) ledgerRun {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte(fakeClaudeStdin), 0o755); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(dir, "stdin.log")
	t.Setenv("STDIN_LOG", stdinLog)
	t.Setenv("FAKE_SESSION_ID", reportedSession)
	ledger := NewSessionLedger(entries)
	task.Command, task.WorkDir, task.UserPrompt, task.SessionLedger = script, dir, "continue the audit", ledger
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, io.Discard)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = b.Execute(ctx, task)
	raw, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("the stand-in CLI was never spawned: %v", err)
	}
	return ledgerRun{stdin: string(raw), ledger: ledger}
}

// The wiring, which the helpers' own tests cannot show: Execute must SEND the
// note for the session it resumes, and settle the ledger once the process ran.
func TestExecuteTellsTheResumedSessionWhatDiedAndSettlesIt(t *testing.T) {
	run := runLedgerExecute(t, Task{SessionID: "s1"}, map[string][]string{"s1": {"auditor (local_agent, t1)"}, "other": {"x"}}, "s1")
	if !strings.Contains(run.stdin, "This session resumes in a new process") || !strings.Contains(run.stdin, "auditor (local_agent, t1)") {
		t.Fatalf("the prompt sent did not carry the note:\n%s", run.stdin)
	}
	if got := run.ledger.Terminated("s1"); got != nil {
		t.Fatalf("s1 = %v after a process that was told and left nothing, want cleared", got)
	}
	if got := run.ledger.Terminated("other"); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("another session's entry = %v, want it untouched", got)
	}
}

// A fork resumes the parent's transcript under a new id: it is told the
// parent's dead work, and the parent keeps its entry for the next fork.
func TestExecuteForkIsToldTheParentsDeadWorkAndLeavesItsEntry(t *testing.T) {
	probe := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	_, fingerprint, _, err := probe.setupCredsAndSession(context.Background(), Task{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{SessionID: "parent", ForkSession: true, SessionFingerprint: fingerprint}
	run := runLedgerExecute(t, task, map[string][]string{"parent": {"auditor (local_agent, t1)"}}, "child")
	if !strings.Contains(run.stdin, "auditor (local_agent, t1)") {
		t.Fatalf("the fork's prompt did not carry the parent's dead work:\n%s", run.stdin)
	}
	if got := run.ledger.Terminated("parent"); !reflect.DeepEqual(got, []string{"auditor (local_agent, t1)"}) {
		t.Fatalf("parent = %v, want its entry kept: the fork did not touch its transcript", got)
	}
	if got := run.ledger.Terminated("child"); got != nil {
		t.Fatalf("child = %v, want nothing: it left no work running", got)
	}
}

// A fork the fingerprint guard drops runs a fresh transcript: the parent's
// dead work is not in it.
func TestExecuteDroppedForkIsToldNothing(t *testing.T) {
	task := Task{SessionID: "parent", ForkSession: true}
	run := runLedgerExecute(t, task, map[string][]string{"parent": {"auditor (local_agent, t1)"}}, "fresh")
	if strings.Contains(run.stdin, "This session resumes in a new process") {
		t.Fatalf("a fresh transcript was told about work it never launched:\n%s", run.stdin)
	}
}

// The formatting pass resumes pass 1's transcript in a process of its own.
func TestTheFormattingPassIsToldWhatDiedWithPassOne(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "argv")
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"" + capture + "\"; done\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"{}\",\"num_turns\":1,\"duration_ms\":1,\"duration_api_ms\":1,\"session_id\":\"s1\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := NewSessionLedger(map[string][]string{"s1": {"auditor (local_agent, t1)"}})
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := Task{Command: script, WorkDir: dir, OutputSchema: []byte(`{"type":"object"}`), SessionLedger: ledger}
	if _, _, _, err := b.formatOutput(ctx, task, "s1"); err != nil {
		t.Fatalf("formatOutput: %v", err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "auditor (local_agent, t1)") {
		t.Fatalf("the formatting prompt did not carry the note:\n%s", raw)
	}
	// A one-shot pass formats what the agent has: a task it re-launched would
	// hold its process for a result it does not need.
	if !strings.Contains(string(raw), "Do not re-launch them") || strings.Contains(string(raw), "Re-launch those") {
		t.Fatalf("the formatting prompt invites a re-launch:\n%s", raw)
	}
	if got := ledger.Terminated("s1"); got != nil {
		t.Fatalf("s1 = %v after the formatting pass was told, want cleared", got)
	}
}

// The formatting pass runs with the node's tools (it is the --max-turns
// fallback of a first pass cut off mid-run): what it launches and never sees
// report back is lost with its process, and recorded like pass 1's — secrets
// masked, in the ledger and in what the call hands back.
func TestTheFormattingPassRecordsTheWorkItLost(t *testing.T) {
	const secret = "sk-live-4f9a8b7c6d5e"
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(fake, []byte(fakeClaudeScripted), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "script")
	script := []string{
		lnInit("2.1.280"),
		lnAssistant("f1", "", cToolUse("tuB", "Bash", map[string]any{"command": "API_KEY=" + secret + " ./watch", "run_in_background": true})),
		lnSnapshotOf(map[string]any{"task_id": "b9", "task_type": "local_bash", "description": "API_KEY=" + secret + " ./watch"}),
		lnToolResult("tuB", "Command running in background with ID: b9", false, ""),
		lnAssistant("f2", "", cToolUse("tuSO", "StructuredOutput", map[string]any{"answer": "x"})),
		lnToolResult("tuSO", "Structured output provided successfully", false, ""),
		lnResult(resultSpec{text: `{"answer":"x"}`, turns: 2, cost: 0.01, structured: map[string]any{"answer": "x"}}),
	}
	if err := os.WriteFile(scriptPath, []byte(strings.Join(script, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_SCRIPT", scriptPath)
	ledger := NewSessionLedger(map[string][]string{"s1": {"auditor (local_agent, t1)"}})
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := Task{Command: fake, WorkDir: dir, OutputSchema: []byte(`{"type":"object"}`), SessionLedger: ledger,
		RedactSecrets: func(s string) string { return strings.ReplaceAll(s, secret, "__ITERION_SECRET_API_KEY__") }}
	var events []BackgroundWork
	task.Hooks.OnBackgroundWork = func(w BackgroundWork) { events = append(events, w) }
	rm, _, lost, err := b.formatOutput(ctx, task, "s1")
	if err != nil || rm == nil {
		t.Fatalf("formatOutput: rm=%v err=%v", rm, err)
	}
	want := []string{"API_KEY=__ITERION_SECRET_API_KEY__ ./watch (local_bash, b9)"}
	if !reflect.DeepEqual(lost, want) {
		t.Fatalf("lost = %q, want %q", lost, want)
	}
	if got := ledger.Terminated("s1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("s1 = %q, want the told entry settled and the pass's lost work recorded: %q", got, want)
	}
	if len(events) != 1 || events[0].Phase != BackgroundAbandoned || events[0].Backend != BackendClaudeCode || !reflect.DeepEqual(events[0].Tasks, want) {
		t.Fatalf("events = %+v, want one abandoned event naming %q", events, want)
	}
}

// A process that dies on a hook event — which already names the session —
// before the CLI announced the turn never read the note: the next process of
// that session must still be told.
func TestAProcessThatDiedBeforeItsTurnDoesNotSettleWhatItWasTold(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\nwhile IFS= read -r line; do case \"$line\" in *'\"type\":\"user\"'*) break ;; esac; done\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"hook_started\",\"hook_name\":\"SessionStart\",\"session_id\":\"s1\"}'\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := NewSessionLedger(map[string][]string{"s1": {"auditor (local_agent, t1)"}})
	task := Task{Command: script, WorkDir: dir, UserPrompt: "continue", SessionID: "s1", SessionLedger: ledger}
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, io.Discard)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := b.Execute(ctx, task); err == nil {
		t.Fatal("Execute succeeded on a CLI that exited before its turn")
	}
	if got := ledger.Terminated("s1"); !reflect.DeepEqual(got, []string{"auditor (local_agent, t1)"}) {
		t.Fatalf("s1 = %v: cleared by a process that never read the note", got)
	}
}

// A formatting pass whose process never started took no prompt: the entry it
// would have been told stays for the next process.
func TestAFormattingPassThatNeverRanSettlesNothing(t *testing.T) {
	dir := t.TempDir()
	ledger := NewSessionLedger(map[string][]string{"s1": {"auditor (local_agent, t1)"}})
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, io.Discard)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := Task{Command: filepath.Join(dir, "no-such-claude"), WorkDir: dir, OutputSchema: []byte(`{"type":"object"}`), SessionLedger: ledger}
	if _, _, _, err := b.formatOutput(ctx, task, "s1"); err == nil {
		t.Fatal("a pass with no CLI binary succeeded")
	}
	if got := ledger.Terminated("s1"); !reflect.DeepEqual(got, []string{"auditor (local_agent, t1)"}) {
		t.Fatalf("s1 = %v: a pass that never ran settled the entry it was told", got)
	}
}

// A label two processes of one session both left behind is recorded — and
// told — once.
func TestSessionLedger_ALabelIsRecordedOnce(t *testing.T) {
	l := NewSessionLedger(nil)
	l.Settle("s", nil, []string{"a", "b"})
	l.Settle("s", nil, []string{"b", "c"})
	if got := l.Terminated("s"); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("entry = %v, want [a b c]", got)
	}
}
