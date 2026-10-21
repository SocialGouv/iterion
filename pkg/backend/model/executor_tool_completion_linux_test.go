//go:build linux

package model

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// execClientRun is a sandbox.Run standing in for an exec client in front of
// a container whose processes are this host's own. What the "container" runs
// is started DETACHED (setsid), so it outlives the client the way a command
// in a pod outlives a `kubectl exec` that ended; and the sweep the engine runs
// through Command scans this host's /proc as it would scan the pod's.
type execClientRun struct {
	// client is how the stand-in client ends:
	//   "waits" — it waits for the command and exits with its status: a
	//             healthy stream. Cancelled, it dies alone, as kubectl does;
	//   "lost"  — it starts the command, waits until running is written to,
	//             and exits 0 with nothing on stdout: the signature of a
	//             stream lost behind kubectl (exit 0, an empty result, the
	//             command still running);
	//   "lies"  — it waits for the command, then exits 0 whatever its status.
	client string
	dir    string
	// running is a file the command writes once it runs: a lost stream
	// ends only once the command provably runs.
	running string
}

func (f *execClientRun) Driver() string { return "fake-exec-client" }

func (f *execClientRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *osexec.Cmd {
	if len(argv) < 4 || argv[0] != "sh" || argv[3] != "iterion-tool" {
		// The sweep, or any other exec: it runs in the "container" itself.
		c := osexec.CommandContext(ctx, argv[0], argv[1:]...)
		c.Dir = f.dir
		return c
	}
	var standIn string
	switch f.client {
	case "waits":
		standIn = `setsid -w "$@" & wait $!`
	case "lost":
		standIn = `setsid "$@" >/dev/null 2>&1 </dev/null &
i=0; while [ ! -s ` + shellQuoteForTest(f.running) + ` ] && [ "$i" -lt 200 ]; do sleep 0.05; i=$((i+1)); done
echo 'exec session ended' >&2; exit 0`
	case "lies":
		standIn = `"$@"; exit 0`
	default:
		panic("unknown client " + f.client)
	}
	c := osexec.CommandContext(ctx, "sh", append([]string{"-c", standIn, "exec-client"}, argv...)...)
	c.Dir = f.dir
	// The exec's own environment, as the pod's exec carries it.
	c.Env = os.Environ()
	for k, v := range opts.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdin = opts.Stdin
	// The kubernetes driver's own bound: a cancelled client is not waited
	// for past it, whatever still holds its output.
	c.WaitDelay = 2 * time.Second
	return c
}

func (f *execClientRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	out, err := f.Command(ctx, argv, opts).Output()
	return sandbox.ExecResult{Stdout: out}, err
}

func (f *execClientRun) Cleanup(context.Context) error { return nil }

func shellQuoteForTest(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func requireSetsid(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("setsid"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("setsid is missing: the exec-client stand-in cannot detach the command (%v)", err)
		}
		t.Skipf("setsid is missing: %v", err)
	}
}

// sandboxedToolExecutor returns an executor whose sandbox is an execClientRun,
// and the heartbeat file a grandchildRecipe of the test writes to.
func sandboxedToolExecutor(t *testing.T, client string) (*ClawExecutor, string) {
	t.Helper()
	requireSetsid(t)
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	e.SetSandbox(&execClientRun{client: client, dir: dir, running: heart})
	return e, heart
}

// The observed failure: a gate script ran 52 minutes into a 3.5-hour run when
// its exec session ended with status 0 and an empty stdout; the node finished
// with {result: ""} and no error, and the script went on applying mutants to
// the workspace the next node worked in. The end of a sandboxed command is
// proven by its completion line, or the node fails — and what it left running
// is stopped.
func TestSandboxedToolNodeLostStreamFailsLoudAndStopsTheCommand(t *testing.T) {
	for _, mode := range []struct {
		name string
		node func(heart string) *ir.ToolNode
	}{
		{"shell", func(heart string) *ir.ToolNode {
			return &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: grandchildRecipe(heart) + `; echo '{"verdict":"pass"}'`}
		}},
		{"script", func(heart string) *ir.ToolNode {
			return &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Language: "sh", Script: grandchildRecipe(heart) + "\necho '{\"verdict\":\"pass\"}'\n"}
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			e, heart := sandboxedToolExecutor(t, "lost")
			out, err := e.Execute(context.Background(), mode.node(heart), map[string]any{})
			if fileSize(t, heart) == 0 {
				t.Fatal("the command never ran: the lost stream proves nothing")
			}
			if err == nil {
				t.Fatalf("a lost exec stream was taken for a finished command: output %v", out)
			}
			if !errors.Is(err, sandbox.ErrExecStatusUnknown) {
				t.Fatalf("the node failed, but not on its unproven end: %v", err)
			}
			if !strings.Contains(err.Error(), "the sweep stopped") {
				t.Fatalf("the failure does not say what the sweep stopped: %v", err)
			}
			assertGrandchildStopped(t, heart)
		})
	}
}

// A healthy stream: the completion is invisible — the output is the
// command's, byte for byte, and a failing command fails on its own status.
func TestSandboxedToolNodeCompletionIsTransparent(t *testing.T) {
	e, _ := sandboxedToolExecutor(t, "waits")
	out, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "ok"},
		Command:  `printf '{"verdict":"pass"}'`,
	}, map[string]any{})
	if err != nil {
		t.Fatalf("a finished command failed: %v", err)
	}
	if out["verdict"] != "pass" {
		t.Fatalf("the command's own output did not come back: %v", out)
	}

	out, err = e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "text"},
		Command:  `printf 'two\nlines\n'`,
	}, map[string]any{})
	if err != nil {
		t.Fatalf("a finished command failed: %v", err)
	}
	if out["result"] != "two\nlines" {
		t.Fatalf("stdout was not the command's own: %q", out["result"])
	}

	_, err = e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "fails"},
		Command:  `echo out; exit 3`,
	}, map[string]any{})
	if err == nil {
		t.Fatal("a command that exited 3 succeeded")
	}
	if errors.Is(err, sandbox.ErrExecStatusUnknown) {
		t.Fatalf("a command whose end arrived was reported unproven: %v", err)
	}
}

// The exec client's status is not the verdict: a client that reports 0 for a
// command that exited 3 does not make the node succeed.
func TestSandboxedToolNodeClientStatusIsNotTheVerdict(t *testing.T) {
	e, _ := sandboxedToolExecutor(t, "lies")
	_, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "fails"},
		Command:  `exit 3`,
	}, map[string]any{})
	if err == nil {
		t.Fatal("a command that exited 3 succeeded because its exec client said 0")
	}
	if !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("the failure does not name the command's own status: %v", err)
	}
}

// Cancelled, a sandboxed node stops what it started: the client dies alone,
// as kubectl does, and the command's session is swept. The node keeps the
// error of its cancel: the cancel explains the missing completion line.
func TestSandboxedToolNodeCancelStopsTheCommand(t *testing.T) {
	e, heart := sandboxedToolExecutor(t, "waits")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(ctx, &ir.ToolNode{BaseNode: ir.BaseNode{ID: "cancelled"}, Command: grandchildRecipe(heart)}, map[string]any{})
		done <- err
	}()
	waitForHeartbeat(t, heart)
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Execute did not return within 15s of cancellation")
	}
	if err == nil {
		t.Fatal("a cancelled node succeeded")
	}
	if errors.Is(err, sandbox.ErrExecStatusUnknown) {
		t.Fatalf("a cancelled node was reported as an unproven end: %v", err)
	}
	assertGrandchildStopped(t, heart)
}

// Every tool node command the sandbox runs is built by sandboxToolCommand, so
// none runs without its completion: a builder that called the sandbox itself
// would be a sandboxed command whose end nothing proves.
func TestSandboxToolCommandIsTheOnlyRouteIntoTheSandbox(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Command" {
					return true
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "sandbox" {
					sites = append(sites, fset.Position(call.Pos()).String()+" in "+fn.Name.Name)
				}
				return true
			})
		}
	}
	if len(sites) != 1 || !strings.HasSuffix(sites[0], " in sandboxToolCommand") {
		t.Fatalf("the sandbox's Command is called outside sandboxToolCommand: %v", sites)
	}
}
