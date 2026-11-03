package delegate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestOpenCodeSessionArgv pins how a resume and a fork reach opencode: the
// session flags follow `run`, which stays the token that opens argv.
func TestOpenCodeSessionArgv(t *testing.T) {
	b := &CLIAgentBackend{Protocol: opencodeProtocol}
	cases := []struct {
		name string
		task Task
		want []string
	}{
		{
			name: "fresh session names none",
			task: Task{UserPrompt: "x"},
			want: []string{"run", "--format", "json"},
		},
		{
			name: "resume continues the named session",
			task: Task{UserPrompt: "x", SessionID: "ses_1"},
			want: []string{"run", "--format", "json", "--session", "ses_1"},
		},
		{
			name: "fork branches off the named session",
			task: Task{UserPrompt: "x", SessionID: "ses_1", ForkSession: true},
			want: []string{"run", "--format", "json", "--session", "ses_1", "--fork=true"},
		},
		{
			// opencode exits 1 on a bare --fork ("--fork requires --continue
			// or --session"); a fork with nothing to fork from is a fresh run.
			name: "fork without a session is a fresh run",
			task: Task{UserPrompt: "x", ForkSession: true},
			want: []string{"run", "--format", "json"},
		},
		{
			name: "the model reference carries the variant after the subcommand",
			task: Task{UserPrompt: "x", Model: "scripted/m1", ReasoningEffort: "high", SessionID: "ses_1"},
			want: []string{"run", "--format", "json", "-m", "scripted/m1#high", "--session", "ses_1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := b.buildArgs(opencodeProtocol, tc.task, "x", "")
			if !reflect.DeepEqual(args, tc.want) {
				t.Fatalf("args = %v, want %v", args, tc.want)
			}
		})
	}
}

// fakeOpenCodeSessions writes an `opencode` stand-in for the 1.x line, which
// iterion drives under the ITERION_OPENCODE_ARGV=1 hatch. It keeps sessions
// the way opencode 1.19's `run` command resolves them (cli/cmd/run.ts,
// measured against the real 1.19.0 binary):
//
//   - no --session: a new session, its id reported on every JSON event;
//   - --session <id>: the stored conversation is continued, and an unknown id
//     is "Session not found" on stderr with exit status 1;
//   - --session <id> --fork: a NEW session seeded with the stored
//     conversation, the original left untouched; --fork alone exits 1.
//
// It also keeps the real parser's trap: the subcommand is found before
// `run`'s own flags are known, so a bare `--fork` followed by a word takes
// that word as its value — `--fork run` loses the subcommand and the real CLI
// prints its help with exit status 1.
//
// The answer is the whole conversation the CLI holds for the session, one
// turn per prompt, so a caller can tell a rehydrated session from a fresh one.
// Anything after `run` fails the call, which is what makes this fake refuse
// the 2.x argv: under the hatch iterion keeps the subcommand last.
func fakeOpenCodeSessions(t *testing.T) (bin, store string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	dir := t.TempDir()
	store = filepath.Join(dir, "sessions")
	if err := os.Mkdir(store, 0o750); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
store='` + store + `'
session=''
fork=0
ran=0
while [ $# -gt 0 ]; do
	if [ "$ran" = 1 ]; then
		echo "argv continues after run: $*" >&2
		exit 2
	fi
	case "$1" in
	--session) session="$2"; shift 2 ;;
	--fork=true) fork=1; shift ;;
	--fork)
		fork=1
		case "${2-}" in
		''|-*) shift ;;
		*) shift 2 ;;
		esac
		;;
	--format|-m|--variant) shift 2 ;;
	run) ran=1; shift ;;
	*) echo "unexpected argument: $1" >&2; exit 2 ;;
	esac
done
if [ "$ran" != 1 ]; then
	printf 'Commands:\n  opencode run [message..]     run opencode with a message\n' >&2
	exit 1
fi
if [ "$fork" = 1 ] && [ -z "$session" ]; then
	echo "--fork requires --continue or --session" >&2
	exit 1
fi
prompt=$(cat)
next="ses_fake$(($(ls "$store" | wc -l) + 1))"
if [ -z "$session" ]; then
	id="$next"
	: > "$store/$id"
elif [ ! -f "$store/$session" ]; then
	echo "Error: Session not found" >&2
	exit 1
elif [ "$fork" = 1 ]; then
	id="$next"
	cp "$store/$session" "$store/$id"
else
	id="$session"
fi
printf '%s\n' "$prompt" >> "$store/$id"
answer=$(paste -sd'|' "$store/$id")
printf '{"type":"text","sessionID":"%s","part":{"type":"text","text":"%s"}}\n' "$id" "$answer"
printf '{"type":"step_finish","sessionID":"%s","part":{"tokens":{"input":3,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}}\n' "$id"
`
	bin = filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { // #nosec G306 — test fixture must be executable
		t.Fatal(err)
	}
	return bin, store
}

// fakeOpenCode2xSessions writes an `opencode` stand-in for the 2.x line,
// modelled on the parser measured on the real 2.2.5 binary:
//
//   - the SUBCOMMAND is resolved first: a flag that precedes `run` is
//     "Unrecognized flag: <flag> in command opencode" on stderr with the
//     usage text on stdout, exit 1 — nothing runs;
//   - `run`'s own flags follow it: --format, -m/--model, --session/-s,
//     --fork (bare or =true), --title, --agent;
//   - `--variant` exists nowhere, so it lands in the unknown-flag arm;
//   - the variant rides the model reference, `provider/model#variant`, and a
//     reference with no model is "Invalid model reference: <ref>";
//   - a model no provider serves is `provider.no-route` / "Model
//     unavailable: <ref>" — this fake serves `scripted/…` only;
//   - `--session <id>` continues that session or CREATES it under the id, so
//     a resume whose transcript is gone is an empty conversation, not a
//     failure (the 1.x line's "Session not found" is gone);
//   - `--fork` with no session is "--fork requires --continue or --session".
//
// Every refusal the CLI encodes in its stream travels on STDOUT with an EMPTY
// stderr, which is the measured 2.x envelope.
//
// argv is recorded one line per invocation, so a test can assert what the CLI
// actually received rather than what buildArgs returned.
func fakeOpenCode2xSessions(t *testing.T) (bin, store, argvLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	dir := t.TempDir()
	store = filepath.Join(dir, "sessions")
	if err := os.Mkdir(store, 0o750); err != nil {
		t.Fatal(err)
	}
	argvLog = filepath.Join(dir, "argv")
	script := `#!/bin/sh
store='` + store + `'
printf '%s\n' "$*" >> '` + argvLog + `'
fail() {
	printf '{"type":"error","timestamp":1,"sessionID":"","error":{"type":"%s","message":"%s"}}\n' "$1" "$2"
	exit 1
}
if [ "${1-}" != "run" ]; then
	printf 'DESCRIPTION\n  OpenCode command line interface\n\nUSAGE\n  opencode <subcommand> [flags] [<directory>]\n'
	printf '\nERRORS\n  Unrecognized flag: %s in command opencode\n' "${1-}" >&2
	exit 1
fi
shift
session=''
fork=0
model=''
while [ $# -gt 0 ]; do
	case "$1" in
	--session|-s) session="$2"; shift 2 ;;
	--fork|--fork=true) fork=1; shift ;;
	-m|--model) model="$2"; shift 2 ;;
	--format|--agent|--title) shift 2 ;;
	*) printf '\nERROR\n  Unrecognized flag: %s in command opencode run\n' "$1" >&2; exit 1 ;;
	esac
done
case "$model" in
'') ;;
'#'*) fail unknown "Invalid model reference: $model" ;;
scripted/*) ;;
*) fail provider.no-route "Model unavailable: $model" ;;
esac
if [ "$fork" = 1 ] && [ -z "$session" ]; then
	fail unknown "--fork requires --continue or --session"
fi
prompt=$(cat)
if [ -z "$prompt" ]; then
	fail unknown "You must provide a message"
fi
next="ses_fake$(($(ls "$store" | wc -l) + 1))"
if [ -z "$session" ]; then
	id="$next"
	: > "$store/$id"
else
	id="$session"
	[ -f "$store/$id" ] || : > "$store/$id"
	if [ "$fork" = 1 ]; then
		id="$next"
		cp "$store/$session" "$store/$id"
	fi
fi
printf '%s\n' "$prompt" >> "$store/$id"
answer=$(paste -sd'|' "$store/$id")
printf '{"type":"text","sessionID":"%s","part":{"type":"text","text":"%s"}}\n' "$id" "$answer"
printf '{"type":"step_finish","sessionID":"%s","part":{"tokens":{"input":3,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}}\n' "$id"
`
	bin = filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { // #nosec G306 — test fixture must be executable
		t.Fatal(err)
	}
	return bin, store, argvLog
}

// TestOpenCodeArgvDialectReachesTheCLI is the end-to-end dialect proof: each
// fake REFUSES the other dialect's argv the way its real CLI does, so a
// backend that spoke the wrong one could not pass.
func TestOpenCodeArgvDialectReachesTheCLI(t *testing.T) {
	run := func(t *testing.T, bin string, task Task) (Result, error) {
		t.Helper()
		ws := t.TempDir()
		if err := os.Mkdir(filepath.Join(ws, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
		task.NodeID, task.WorkDir, task.BaseDir = "n1", ws, ws
		return NewOpenCodeBackend(testLogger(), bin).Execute(context.Background(), task)
	}

	t.Run("2.x is the default, and argv reaches the CLI subcommand-first", func(t *testing.T) {
		bin, _, argvLog := fakeOpenCode2xSessions(t)
		if _, err := run(t, bin, Task{UserPrompt: "x", Model: "scripted/m1", ReasoningEffort: "ultracode"}); err != nil {
			t.Fatalf("the 2.x fake refused iterion's argv: %v", err)
		}
		if got := strings.TrimSpace(readFileString(t, argvLog)); got != "run --format json -m scripted/m1#high" {
			t.Fatalf("the CLI received %q, want the 2.x argv", got)
		}
	})

	t.Run("the 1.19 fake refuses the 2.x argv", func(t *testing.T) {
		bin, _ := fakeOpenCodeSessions(t)
		res, err := run(t, bin, Task{UserPrompt: "x"})
		if err == nil {
			t.Fatalf("the 1.19 fake accepted the 2.x argv: %v", res.Output)
		}
		if !strings.Contains(res.Stderr, "argv continues after run") {
			t.Fatalf("Stderr = %q, want the 1.x parser's refusal", res.Stderr)
		}
	})

	t.Run("the hatch re-emits the 1.x argv", func(t *testing.T) {
		t.Setenv(openCodeLegacyArgvEnv, "1")
		bin, _ := fakeOpenCodeSessions(t)
		if _, err := run(t, bin, Task{UserPrompt: "x", Model: "scripted/m1", ReasoningEffort: "high"}); err != nil {
			t.Fatalf("the 1.19 fake refused the hatched argv: %v", err)
		}
	})

	t.Run("the 2.x fake refuses the hatched argv", func(t *testing.T) {
		t.Setenv(openCodeLegacyArgvEnv, "1")
		bin, _, _ := fakeOpenCode2xSessions(t)
		res, err := run(t, bin, Task{UserPrompt: "x"})
		if err == nil {
			t.Fatalf("the 2.x fake accepted the 1.x argv: %v", res.Output)
		}
		if !strings.Contains(res.Stderr, "Unrecognized flag: --format in command opencode") {
			t.Fatalf("Stderr = %q, want the 2.x parser's refusal", res.Stderr)
		}
	})
}

// TestOpenCode2xSessionRoundTrip drives Execute through a resume and a fork on
// the 2.x line: the session id the stream reports must be accepted by the next
// invocation, and what that invocation answers from must be the earlier
// conversation.
func TestOpenCode2xSessionRoundTrip(t *testing.T) {
	bin, store, _ := fakeOpenCode2xSessions(t)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	b := NewOpenCodeBackend(testLogger(), bin)
	turn := func(t *testing.T, prompt, sessionID string, fork bool) (Result, error) {
		t.Helper()
		return b.Execute(context.Background(), Task{
			NodeID:      "n1",
			WorkDir:     ws,
			BaseDir:     ws,
			UserPrompt:  prompt,
			SessionID:   sessionID,
			ForkSession: fork,
		})
	}

	first, err := turn(t, "turn-alpha", "", false)
	if err != nil {
		t.Fatalf("fresh turn: %v", err)
	}
	if first.SessionID == "" {
		t.Fatal("the fresh turn reported no session id")
	}

	resumed, err := turn(t, "turn-beta", first.SessionID, false)
	if err != nil {
		t.Fatalf("resume of %q: %v", first.SessionID, err)
	}
	if resumed.SessionID != first.SessionID {
		t.Fatalf("resume reported session %q, want the resumed %q", resumed.SessionID, first.SessionID)
	}
	if got := resumed.Output["text"]; got != "turn-alpha|turn-beta" {
		t.Fatalf("resumed turn answered from %q, want the whole conversation %q", got, "turn-alpha|turn-beta")
	}

	forked, err := turn(t, "turn-gamma", first.SessionID, true)
	if err != nil {
		t.Fatalf("fork of %q: %v", first.SessionID, err)
	}
	if forked.SessionID == "" || forked.SessionID == first.SessionID {
		t.Fatalf("fork reported session %q, want a new id distinct from %q", forked.SessionID, first.SessionID)
	}
	if got := forked.Output["text"]; got != "turn-alpha|turn-beta|turn-gamma" {
		t.Fatalf("forked turn answered from %q, want the parent conversation plus its own turn", got)
	}
	parent, err := os.ReadFile(filepath.Join(store, first.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(parent), "turn-gamma") {
		t.Fatalf("the fork wrote into its parent session: %q", parent)
	}
}

// TestOpenCode2xResumeOfAnUnknownSessionStartsIt: on the 2.x line a --session
// the CLI does not hold is CREATED under that id (measured on 2.2.5), so a
// resume whose transcript is gone succeeds as an EMPTY conversation. The
// session id must still be the one asked for, which is what tells a caller
// the transcript did not come back.
func TestOpenCode2xResumeOfAnUnknownSessionStartsIt(t *testing.T) {
	bin, _, _ := fakeOpenCode2xSessions(t)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	res, err := NewOpenCodeBackend(testLogger(), bin).Execute(context.Background(), Task{
		NodeID:     "n1",
		WorkDir:    ws,
		BaseDir:    ws,
		UserPrompt: "turn-alpha",
		SessionID:  "ses_gone",
	})
	if err != nil {
		t.Fatalf("a resume of an unknown session failed: %v (stderr %q)", err, res.Stderr)
	}
	if res.SessionID != "ses_gone" {
		t.Fatalf("SessionID = %q, want the requested ses_gone", res.SessionID)
	}
	if got := res.Output["text"]; got != "turn-alpha" {
		t.Fatalf("the created session answered from %q, want its own turn alone", got)
	}
}

// TestOpenCodeEffortWithoutAModelIsWarned: the 2.x variant rides the model
// reference, so an effort with no model cannot be spelled. The run still
// happens — the dial is dropped, named in a warning, never silently.
func TestOpenCodeEffortWithoutAModelIsWarned(t *testing.T) {
	bin, _, argvLog := fakeOpenCode2xSessions(t)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	b := NewOpenCodeBackend(iterlog.New(iterlog.LevelWarn, &logBuf), bin)
	if _, err := b.Execute(context.Background(), Task{
		NodeID:          "n1",
		WorkDir:         ws,
		BaseDir:         ws,
		UserPrompt:      "x",
		ReasoningEffort: "high",
	}); err != nil {
		t.Fatalf("the run was rejected instead of warned: %v", err)
	}
	if got := logBuf.String(); !strings.Contains(got, "reasoning_effort \"high\"") || !strings.Contains(got, BackendOpenCode) {
		t.Fatalf("logs = %q, want a warning naming the dropped dial", got)
	}
	if got := strings.TrimSpace(readFileString(t, argvLog)); got != "run --format json" {
		t.Fatalf("the CLI received %q, want no model reference at all", got)
	}
}

// TestOpenCodeFailureReasonTravelsOnStdout: on the 2.x line opencode reports
// WHY in its JSON stream on STDOUT and leaves stderr empty, even when the
// process exits non-zero (measured on 2.2.5: a model no provider serves →
// `{"error":{"type":"provider.no-route","message":"Model unavailable: …"}}`,
// exit 1, no stderr). The node must carry that sentence, not `exit status 1`.
func TestOpenCodeFailureReasonTravelsOnStdout(t *testing.T) {
	bin, _, _ := fakeOpenCode2xSessions(t)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	res, err := NewOpenCodeBackend(testLogger(), bin).Execute(context.Background(), Task{
		NodeID:     "n1",
		WorkDir:    ws,
		BaseDir:    ws,
		UserPrompt: "x",
		Model:      "nosuch/model",
	})
	if err == nil {
		t.Fatalf("a model no provider serves succeeded with %v", res.Output)
	}
	if !strings.Contains(err.Error(), "Model unavailable: nosuch/model") {
		t.Fatalf("error = %v, want the CLI's own reason from its stdout stream", err)
	}
	if strings.TrimSpace(res.Stderr) != "" {
		t.Fatalf("Stderr = %q, want empty — the reason travelled on stdout alone", res.Stderr)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 — a path the test itself created.
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestOpenCodeSessionRoundTrip drives Execute through a resume and a fork on
// the 1.x line, under the ITERION_OPENCODE_ARGV=1 hatch: the session id the
// stream reports must be accepted by the next invocation, and what that
// invocation answers from must be the earlier conversation.
func TestOpenCodeSessionRoundTrip(t *testing.T) {
	t.Setenv(openCodeLegacyArgvEnv, "1")
	bin, store := fakeOpenCodeSessions(t)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	b := NewOpenCodeBackend(testLogger(), bin)
	turn := func(t *testing.T, prompt, sessionID string, fork bool) (Result, error) {
		t.Helper()
		return b.Execute(context.Background(), Task{
			NodeID:      "n1",
			WorkDir:     ws,
			BaseDir:     ws,
			UserPrompt:  prompt,
			SessionID:   sessionID,
			ForkSession: fork,
		})
	}

	first, err := turn(t, "turn-alpha", "", false)
	if err != nil {
		t.Fatalf("fresh turn: %v", err)
	}
	if first.SessionID == "" {
		t.Fatal("the fresh turn reported no session id")
	}

	resumed, err := turn(t, "turn-beta", first.SessionID, false)
	if err != nil {
		t.Fatalf("resume of %q: %v", first.SessionID, err)
	}
	if resumed.SessionID != first.SessionID {
		t.Fatalf("resume reported session %q, want the resumed %q", resumed.SessionID, first.SessionID)
	}
	if got := resumed.Output["text"]; got != "turn-alpha|turn-beta" {
		t.Fatalf("resumed turn answered from %q, want the whole conversation %q", got, "turn-alpha|turn-beta")
	}

	forked, err := turn(t, "turn-gamma", first.SessionID, true)
	if err != nil {
		t.Fatalf("fork of %q: %v", first.SessionID, err)
	}
	if forked.SessionID == "" || forked.SessionID == first.SessionID {
		t.Fatalf("fork reported session %q, want a new id distinct from %q", forked.SessionID, first.SessionID)
	}
	if got := forked.Output["text"]; got != "turn-alpha|turn-beta|turn-gamma" {
		t.Fatalf("forked turn answered from %q, want the parent conversation plus its own turn", got)
	}
	parent, err := os.ReadFile(filepath.Join(store, first.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(parent), "turn-gamma") {
		t.Fatalf("the fork wrote into its parent session: %q", parent)
	}
}

// TestOpenCodeResumeOfAnUnknownSessionFails: opencode 1.19 refuses a
// --session it does not hold, so under the 1.x hatch a resume whose
// transcript is gone surfaces as a failed call — never as a silently fresh
// conversation. The 2.x line answers the other way, which is its own test.
func TestOpenCodeResumeOfAnUnknownSessionFails(t *testing.T) {
	t.Setenv(openCodeLegacyArgvEnv, "1")
	bin, _ := fakeOpenCodeSessions(t)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	res, err := NewOpenCodeBackend(testLogger(), bin).Execute(context.Background(), Task{
		NodeID:     "n1",
		WorkDir:    ws,
		BaseDir:    ws,
		UserPrompt: "turn-alpha",
		SessionID:  "ses_gone",
	})
	if err == nil {
		t.Fatalf("a resume of an unknown session succeeded with %v", res.Output)
	}
	if !strings.Contains(res.Stderr, "Session not found") {
		t.Fatalf("Stderr = %q, want the CLI's own reason", res.Stderr)
	}
}
