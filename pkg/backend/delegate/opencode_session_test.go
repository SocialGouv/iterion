package delegate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// TestOpenCodeSessionArgv pins how a resume and a fork reach opencode: the
// session flags precede `run`, which stays the token that closes argv.
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
			want: []string{"--format", "json", "run"},
		},
		{
			name: "resume continues the named session",
			task: Task{UserPrompt: "x", SessionID: "ses_1"},
			want: []string{"--format", "json", "--session", "ses_1", "run"},
		},
		{
			name: "fork branches off the named session",
			task: Task{UserPrompt: "x", SessionID: "ses_1", ForkSession: true},
			want: []string{"--format", "json", "--session", "ses_1", "--fork=true", "run"},
		},
		{
			// opencode exits 1 on a bare --fork ("--fork requires --continue
			// or --session"); a fork with nothing to fork from is a fresh run.
			name: "fork without a session is a fresh run",
			task: Task{UserPrompt: "x", ForkSession: true},
			want: []string{"--format", "json", "run"},
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

// fakeOpenCodeSessions writes an `opencode` stand-in that keeps sessions the
// way opencode 1.19's `run` command resolves them (cli/cmd/run.ts, measured
// against the real 1.19.0 binary):
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
// Anything after `run` fails the call: iterion keeps the subcommand last.
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

// TestOpenCodeSessionRoundTrip drives Execute through a resume and a fork:
// the session id the stream reports must be accepted by the next invocation,
// and what that invocation answers from must be the earlier conversation.
func TestOpenCodeSessionRoundTrip(t *testing.T) {
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
// --session it does not hold, so a resume whose transcript is gone surfaces
// as a failed call — never as a silently fresh conversation.
func TestOpenCodeResumeOfAnUnknownSessionFails(t *testing.T) {
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
