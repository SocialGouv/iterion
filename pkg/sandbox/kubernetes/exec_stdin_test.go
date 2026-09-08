package kubernetes

import (
	"context"
	"io"
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

// A tool node whose interpolated recipe exceeds the kernel's
// single-argument cap must not reach `kubectl exec` through argv: the
// fork fails with E2BIG ("argument list too long") before the pod is
// ever contacted, and it fails identically on every retry.
func TestCommand_OversizedScriptStreamsThroughStdin(t *testing.T) {
	r := testRun()
	big := strings.Repeat("x", sandbox.MaxInlineArgBytes+1)
	script := "cat <<'EOF'\n" + big + "\nEOF\n"

	cmd := r.Command(context.Background(), []string{"bash", "-c", script}, sandbox.ExecOpts{})

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

// The custom-workdir path wraps the recipe in `cd <dir> && exec …`, so
// the wrapper is at least as large as the recipe it embeds: it has to
// take the same route, or the fix would only cover half the callers.
func TestCommand_OversizedScriptWithCustomWorkDirStreamsToo(t *testing.T) {
	r := testRun()
	big := strings.Repeat("y", sandbox.MaxInlineArgBytes+1)

	cmd := r.Command(context.Background(), []string{"bash", "-c", big}, sandbox.ExecOpts{WorkDir: "/elsewhere"})

	for _, a := range cmd.Args {
		if strings.Contains(a, big) {
			t.Fatalf("oversized wrapper leaked into argv (E2BIG risk); arg len=%d", len(a))
		}
	}
	if n := len(cmd.Args); n < 2 || cmd.Args[n-2] != "sh" || cmd.Args[n-1] != "-s" {
		t.Fatalf("argv must terminate with `sh -s`; got tail %v", cmd.Args[max(0, len(cmd.Args)-4):])
	}
	got := readStdin(t, cmd.Stdin)
	if !strings.Contains(got, "/elsewhere") {
		t.Errorf("streamed wrapper must still cd into the requested workdir; got %.120q", got)
	}
	if !strings.Contains(got, big) {
		t.Error("streamed wrapper must still carry the recipe")
	}
}

// A caller that attached its own reader keeps it: the streaming path
// must never clobber caller-provided stdin, whatever the script size.
func TestCommand_CallerStdinWins(t *testing.T) {
	r := testRun()
	big := strings.Repeat("z", sandbox.MaxInlineArgBytes+1)
	mine := strings.NewReader("caller payload")

	cmd := r.Command(context.Background(), []string{"bash", "-c", big}, sandbox.ExecOpts{Stdin: mine})

	if got := readStdin(t, cmd.Stdin); got != "caller payload" {
		t.Fatalf("caller stdin must survive; got %.60q", got)
	}
	if n := len(cmd.Args); n >= 2 && cmd.Args[n-1] == "-s" {
		t.Error("with caller stdin attached the script must stay on argv, not take the -s route")
	}
}
