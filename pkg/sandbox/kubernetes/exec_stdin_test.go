package kubernetes

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// testRun is the smallest Run that Command needs: a kubectl path, a pod
// coordinate, and a prepared workspace so the default-workdir branch is
// the one under test.
func testRun() *Run {
	return &Run{
		driver:    &Driver{kubectl: "/usr/local/bin/kubectl"},
		namespace: "ns",
		podName:   "sandbox-x",
		prepared:  &Prepared{workspace: "/ws"},
	}
}

func argvHas(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func readStdin(t *testing.T, r io.Reader) string {
	t.Helper()
	if r == nil {
		return ""
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	return string(b)
}

// assertStdinAnnounced is the invariant every case below asserts, and
// the one the whole streaming design turns on: kubectl opens a stdin
// stream to the pod ONLY when `--stdin` is on its argv, so a reader
// attached without the flag is dropped on the floor — the in-pod
// `<shell> -s` reads EOF, executes nothing, and exits 0, turning an
// oversized payload into a *successful empty run* instead of a loud,
// retryable E2BIG. Asserting it once here catches the whole class,
// not the three instances of it. (The converse, flag without reader,
// is legitimate: a KeepStdinOpen caller wires its own pipe later.)
func assertStdinAnnounced(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Stdin != nil && !argvHas(cmd.Args, "--stdin") {
		t.Fatalf("cmd.Stdin is attached but kubectl was not given --stdin: "+
			"the payload is dropped and the pod exits 0 having run nothing; args=%v", cmd.Args)
	}
}

// A tool node whose interpolated recipe exceeds the kernel's
// single-argument cap must not reach `kubectl exec` through argv: the
// fork fails with E2BIG ("argument list too long") before the pod is
// ever contacted, and it fails identically on every retry.
func TestCommand_OversizedScriptStreamsThroughStdin(t *testing.T) {
	r := testRun()
	big := strings.Repeat("x", sandbox.MaxInlineArgBytes+1)
	script := "cat <<'EOF'\n" + big + "\nEOF\n"

	cmd := r.Command(context.Background(), []string{"bash", "-c", script}, sandbox.ExecOpts{})

	assertStdinAnnounced(t, cmd)
	for _, a := range cmd.Args {
		if strings.Contains(a, big) {
			t.Fatalf("oversized script leaked into argv (E2BIG risk); arg len=%d", len(a))
		}
	}
	if n := len(cmd.Args); n < 2 || cmd.Args[n-2] != "bash" || cmd.Args[n-1] != "-s" {
		t.Fatalf("argv must terminate with `bash -s` (the recipe's own shell); got tail %v", cmd.Args[max(0, len(cmd.Args)-4):])
	}
	if !argvHas(cmd.Args, "--stdin") {
		t.Errorf("kubectl needs --stdin to forward the streamed script; args=%v", cmd.Args)
	}
	if got := readStdin(t, cmd.Stdin); got != script {
		t.Errorf("stdin must carry the script verbatim: got %d bytes, want %d", len(got), len(script))
	}
}

// Below the threshold nothing changes — the argv path is byte-for-byte
// what it was, including the absence of --stdin.
func TestCommand_SmallScriptKeepsArgvPath(t *testing.T) {
	r := testRun()
	script := "echo hello"

	cmd := r.Command(context.Background(), []string{"bash", "-c", script}, sandbox.ExecOpts{})

	assertStdinAnnounced(t, cmd)
	if !argvHas(cmd.Args, script) {
		t.Errorf("small script must stay in argv; args=%v", cmd.Args)
	}
	if argvHas(cmd.Args, "--stdin") {
		t.Errorf("small script must NOT add --stdin (no reader attached); args=%v", cmd.Args)
	}
	if cmd.Stdin != nil {
		t.Errorf("small script must leave Stdin nil, got %T", cmd.Stdin)
	}
}

// A custom workdir over an oversized recipe is BOTH crossings at once.
// Streaming the `cd … && exec bash -c '<script>'` wrapper would only
// move the problem: the in-pod shell re-issues that execve, and the
// kernel's per-argument cap applies there too. So the payload must
// carry the script for the shell to READ, with nothing oversized left
// on any argv — the host's or the pod's.
func TestCommand_OversizedScriptWithCustomWorkDirStreamsTheScriptItself(t *testing.T) {
	r := testRun()
	big := strings.Repeat("y", sandbox.MaxInlineArgBytes+1)

	cmd := r.Command(context.Background(), []string{"bash", "-c", big},
		sandbox.ExecOpts{WorkDir: "/elsewhere", Env: map[string]string{"FOO": "bar baz"}})

	assertStdinAnnounced(t, cmd)
	for _, a := range cmd.Args {
		if strings.Contains(a, big) {
			t.Fatalf("oversized script leaked into host argv (E2BIG risk); arg len=%d", len(a))
		}
	}
	// The recipe's own shell reads it, so bash stays bash.
	if n := len(cmd.Args); n < 2 || cmd.Args[n-2] != "bash" || cmd.Args[n-1] != "-s" {
		t.Fatalf("argv must terminate with `bash -s`; got tail %v", cmd.Args[max(0, len(cmd.Args)-4):])
	}

	got := readStdin(t, cmd.Stdin)
	if !strings.Contains(got, "/elsewhere") {
		t.Errorf("payload must cd into the requested workdir; got %.120q", got)
	}
	if !strings.Contains(got, big) {
		t.Error("payload must carry the recipe")
	}
	if !strings.Contains(got, "export 'FOO=bar baz'") {
		t.Errorf("per-call env must survive as an export line; got %.200q", got)
	}
	// The regression itself: the payload must not hand the script to a
	// nested shell as an argument, which is the execve the pod would
	// then fail on.
	if strings.Contains(got, "exec bash -c") || strings.Contains(got, "bash -c '") {
		t.Errorf("payload re-embeds the script as an argv element — E2BIG merely relocated into the pod; got %.200q", got)
	}
}

// A caller that attached its own reader keeps it: the streaming path
// must never clobber caller-provided stdin, whatever the script size.
func TestCommand_CallerStdinWins(t *testing.T) {
	r := testRun()
	big := strings.Repeat("z", sandbox.MaxInlineArgBytes+1)
	mine := strings.NewReader("caller payload")

	cmd := r.Command(context.Background(), []string{"bash", "-c", big}, sandbox.ExecOpts{Stdin: mine})

	assertStdinAnnounced(t, cmd)
	if got := readStdin(t, cmd.Stdin); got != "caller payload" {
		t.Fatalf("caller stdin must survive; got %.60q", got)
	}
	if n := len(cmd.Args); n >= 2 && cmd.Args[n-1] == "-s" {
		t.Error("with caller stdin attached the script must stay on argv, not take the -s route")
	}
}

// The gap window the shipped custom-workdir branch could not see: a
// script that fits in one argv element but whose WRAPPER does not.
// `buildShellChdirExec` prepends `cd <dir> && exec ` and re-quotes the
// script, expanding every `'` to `'\”` — so a quote-heavy recipe well
// under the cap crosses it once wrapped. The size trigger and the
// `--stdin` flag used to be computed from different values here, which
// streamed the wrapper to a kubectl that never opened the stream.
func TestCommand_WrapperCrossesCapWhileScriptFits(t *testing.T) {
	r := testRun()
	// Quote-heavy, and deliberately just under the cap so the raw
	// per-element predicate cannot fire — the wrapper's own growth is
	// the only thing that crosses it.
	script := strings.Repeat("x", 98_000) + strings.Repeat("'", 800)
	if len(script) > sandbox.MaxInlineArgBytes {
		t.Fatalf("premise broken: script must fit one argv element (%d > %d)", len(script), sandbox.MaxInlineArgBytes)
	}
	cmd3 := []string{"bash", "-c", script}
	wrapper := buildShellChdirExec("/elsewhere", cmd3, nil)
	if len(wrapper) <= sandbox.MaxInlineArgBytes {
		t.Fatalf("premise broken: wrapper must cross the cap (%d <= %d)", len(wrapper), sandbox.MaxInlineArgBytes)
	}

	cmd := r.Command(context.Background(), cmd3, sandbox.ExecOpts{WorkDir: "/elsewhere"})

	assertStdinAnnounced(t, cmd)
	for _, a := range cmd.Args {
		if len(a) > sandbox.MaxInlineArgBytes {
			t.Fatalf("wrapper stayed in argv at %d bytes — E2BIG at fork", len(a))
		}
	}
	// Assert the exact bytes, not a substring: `shellquote` re-escapes
	// the recipe's own quotes on the way in, so the raw script is NOT a
	// substring of the payload — which is the whole reason the wrapper
	// can outgrow the script it carries.
	if got := readStdin(t, cmd.Stdin); got != wrapper {
		t.Errorf("streamed payload must be the wrapper verbatim: got %d bytes, want %d", len(got), len(wrapper))
	}
}

// Same window, non-shell shape: the wrapper flattens EVERY argv element
// into one `sh -c` argument, so a plain binary call with a large flag
// value crosses the cap on kubernetes even though each element fits.
// Docker has no counterpart (it takes a native --workdir), which is why
// this rule is k8s-only and shape-agnostic.
func TestCommand_NonShellShapeWithCustomWorkDirStreams(t *testing.T) {
	r := testRun()
	big := strings.Repeat("q", sandbox.MaxInlineArgBytes+1)

	cmd := r.Command(context.Background(), []string{"my-tool", "--payload", big}, sandbox.ExecOpts{WorkDir: "/elsewhere"})

	assertStdinAnnounced(t, cmd)
	for _, a := range cmd.Args {
		if strings.Contains(a, big) {
			t.Fatalf("oversized wrapper leaked into argv (E2BIG risk); arg len=%d", len(a))
		}
	}
	got := readStdin(t, cmd.Stdin)
	if !strings.Contains(got, big) || !strings.Contains(got, "my-tool") {
		t.Error("streamed wrapper must still carry the full command")
	}
}

// KeepStdinOpen means the caller wires cmd.StdinPipe() afterwards
// (pi_rpc's RPC session, claw_backend's runner, claude_code's session
// mode) — all three also pass a WorkDir. Commandeering their stdin for
// the wrapper would make that pipe fail with "exec: Stdin already set",
// and even if it didn't the program would inherit a consumed reader
// instead of a live pipe. They keep the argv path: a loud, retryable
// E2BIG beats a broken session.
func TestCommand_KeepStdinOpenNeverCommandeered(t *testing.T) {
	r := testRun()
	big := strings.Repeat("k", sandbox.MaxInlineArgBytes+1)

	cmd := r.Command(context.Background(), []string{"my-tool", big}, sandbox.ExecOpts{
		WorkDir:       "/elsewhere",
		KeepStdinOpen: true,
	})

	assertStdinAnnounced(t, cmd)
	if cmd.Stdin != nil {
		t.Fatalf("KeepStdinOpen caller must keep Stdin free for its own pipe, got %T", cmd.Stdin)
	}
	if n := len(cmd.Args); n >= 2 && cmd.Args[n-1] == "-s" {
		t.Error("KeepStdinOpen caller must stay on the argv path, not take the -s route")
	}
	if !argvHas(cmd.Args, "--stdin") {
		t.Errorf("KeepStdinOpen still needs --stdin for the caller's own pipe; args=%v", cmd.Args)
	}
}
