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
	//   "lies"  — it waits for the command, then exits 0 whatever its status;
	//   "truncates" — it waits for the command, and hands it only the first
	//             120,000 bytes of its stdin: a stdin stream cut short.
	client string
	dir    string
	// running is a file the command writes once it runs: a lost stream
	// ends only once the command provably runs.
	running string
	// sweepFails makes every other exec — the sweep — fail, as an exec path
	// still down after the loss of a stream would; sweepFailures fails only
	// the first ones, as a short outage would.
	sweepFails    bool
	sweepFailures int
	// sweeps counts the other execs the engine ran: its sweeps.
	sweeps int
	// clients, when set, is the client of each successive tool command —
	// a postcondition, then a recipe, then a postcondition — before client.
	clients []string
	next    int
	// tools counts the tool commands the engine handed the container.
	tools int
	// path, when set, is the container's PATH for the tool commands.
	path string
}

func (f *execClientRun) Driver() string { return "fake-exec-client" }

func (f *execClientRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *osexec.Cmd {
	if len(argv) < 4 || argv[0] != "sh" || argv[3] != "iterion-tool" {
		// The sweep, or any other exec: it runs in the "container" itself.
		f.sweeps++
		if f.sweepFails || f.sweeps <= f.sweepFailures {
			return osexec.CommandContext(ctx, "sh", "-c", "echo 'error: unable to upgrade connection' >&2; exit 1")
		}
		c := osexec.CommandContext(ctx, argv[0], argv[1:]...)
		c.Dir = f.dir
		return c
	}
	f.tools++
	client := f.client
	if f.next < len(f.clients) {
		client = f.clients[f.next]
		f.next++
	}
	var standIn string
	switch client {
	case "unstartable":
		return osexec.CommandContext(ctx, filepath.Join(f.dir, "no-such-exec-client"))
	case "waits":
		// A background job of a non-interactive sh reads /dev/null: the
		// client's stdin is handed to it explicitly, through fd 3.
		standIn = `exec 3<&0; setsid -w "$@" <&3 3<&- & exec 3<&-; wait $!`
	case "truncates":
		standIn = `head -c 120000 | "$@"`
	case "cuts":
		// The stream is lost once the command's first output has arrived.
		cut := shellQuoteForTest(filepath.Join(f.dir, "cut.out"))
		standIn = `setsid "$@" >` + cut + ` 2>&1 </dev/null &
until [ -s ` + cut + ` ]; do sleep 0.05; done; sleep 0.1; cat ` + cut + `; exit 0`
	case "lost":
		standIn = `setsid "$@" >/dev/null 2>&1 </dev/null &
i=0; while [ ! -s ` + shellQuoteForTest(f.running) + ` ] && [ "$i" -lt 200 ]; do sleep 0.05; i=$((i+1)); done
echo 'exec session ended' >&2; exit 0`
	case "lies":
		standIn = `"$@"; exit 0`
	default:
		panic("unknown client " + client)
	}
	c := osexec.CommandContext(ctx, "sh", append([]string{"-c", standIn, "exec-client"}, argv...)...)
	c.Dir = f.dir
	// The exec's own environment, as the pod's exec carries it.
	c.Env = os.Environ()
	for k, v := range opts.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	if f.path != "" {
		c.Env = append(c.Env, "PATH="+f.path)
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

// Every call into a sandbox run's exec seam in this package is one of the
// listed sites, each for its reason: a tool node's command is built by
// sandboxToolCommand alone, so none runs without its completion. Any other
// value's Command or Exec — a run passed around, an alias of e.sandbox, a
// method value — is a new site, and a new site is a review question, not a
// silent route around the proof.
func TestSandboxToolCommandIsTheOnlyRouteIntoTheSandbox(t *testing.T) {
	allowed := map[string]string{
		"sandboxToolCommand.Command":      "tool nodes: proven by their completion line",
		"executeViaSandboxRunner.Command": "an agent node's claw runner: ended by its ADR-127 session sweep",
		"removeHostCodexForfaitCopy.Exec": "housekeeping: removes a credential copy, decides no node's verdict",
		"scriptRecipe.Exec":               "best-effort removal of the script's in-sandbox copy",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		imports := map[string]bool{}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			local := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imports[local] = true
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var sel *ast.SelectorExpr
				switch n := n.(type) {
				case *ast.CallExpr:
					// A call of a value's Command or Exec method.
					sel, _ = n.Fun.(*ast.SelectorExpr)
				case *ast.SelectorExpr:
					// Any reference to e.sandbox's methods, a method value
					// included: `f := e.sandbox.Command` is a route too.
					if inner, ok := n.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "sandbox" {
						sel = n
					}
				}
				if sel == nil || (sel.Sel.Name != "Command" && sel.Sel.Name != "Exec") {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] {
					return true // a package function, exec.Command and the like
				}
				site := fn.Name.Name + "." + sel.Sel.Name
				if _, ok := allowed[site]; !ok {
					t.Errorf("%s: %s is a call into an exec seam outside the listed sites", fset.Position(sel.Pos()), site)
				}
				seen[site] = true
				return true
			})
		}
	}
	for site := range allowed {
		if !seen[site] {
			t.Errorf("listed site %s is gone: the list must say what the package does", site)
		}
	}
}

// A recipe's own group idioms keep their meaning: the command leads its
// group, as an exec'd command does, and its `kill 0` does not end the
// wrapper that reports its end.
func TestSandboxedToolNodeKillZeroInTheRecipe(t *testing.T) {
	e, _ := sandboxedToolExecutor(t, "waits")
	out, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "cleans-up"},
		Command:  `sleep 30 & trap "" TERM; kill 0; sleep 0.2; echo '{"verdict":"pass"}'`,
	}, map[string]any{})
	if err != nil {
		t.Fatalf("a recipe that ends its own group failed: %v", err)
	}
	if out["verdict"] != "pass" {
		t.Fatalf("the recipe's output did not come back: %v", out)
	}
}

// hostRun is a sandbox driver that runs commands on the engine's own host,
// as the noop driver does; it records the argv it is handed.
type hostRun struct {
	execClientRun
	argvs [][]string
}

func (h *hostRun) HostExecutesCommands() bool { return true }

func (h *hostRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *osexec.Cmd {
	h.argvs = append(h.argvs, append([]string(nil), argv...))
	c := osexec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = h.dir
	return c
}

var _ sandbox.HostRun = (*hostRun)(nil)

// A driver that runs the command on this host hands back the process itself:
// the command runs as it is, unwrapped, and its status is its own.
func TestSandboxedToolNodeOnTheHostRunsTheCommandAsItIs(t *testing.T) {
	dir := t.TempDir()
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	host := &hostRun{execClientRun: execClientRun{dir: dir}}
	e.SetSandbox(host)
	out, err := e.Execute(context.Background(), &ir.ToolNode{BaseNode: ir.BaseNode{ID: "host"}, Command: `printf '{"verdict":"pass"}'`}, map[string]any{})
	if err != nil || out["verdict"] != "pass" {
		t.Fatalf("a host command failed: %v, %v", err, out)
	}
	if len(host.argvs) != 1 || host.argvs[0][0] != "bash" || host.argvs[0][1] != "-c" {
		t.Fatalf("the host driver was handed %v, not the command itself", host.argvs)
	}
}

// A cancel that lands after the client reported success leaves the line to
// decide: no line, or a line with another status, is no success.
func TestSettleJudgesTheLineAfterACancel(t *testing.T) {
	e, _ := sandboxedToolExecutor(t, "waits")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// wrapped runs script under c's protocol here and returns its stdout,
	// completion line included.
	wrapped := func(c *sandbox.Completion, script string) []byte {
		argv := c.Wrap([]string{"sh", "-c", script})
		out, _ := osexec.Command(argv[0], argv[1:]...).Output()
		return out
	}

	c := sandbox.NewCompletion("t")
	if _, err := e.settleSandboxedRun(ctx, "n", c, []byte("partial"), nil); err == nil || !errors.Is(err, context.Canceled) || errors.Is(err, sandbox.ErrExecStatusUnknown) {
		t.Fatalf("no line after a cancel: %v, want the cancel's error", err)
	}

	c = sandbox.NewCompletion("t")
	if _, err := e.settleSandboxedRun(ctx, "n", c, wrapped(c, "printf partial; exit 3"), nil); err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("a line saying 3 after a cancel: %v, want the command's status", err)
	}

	c = sandbox.NewCompletion("t")
	clean, err := e.settleSandboxedRun(ctx, "n", c, wrapped(c, "printf done"), nil)
	if err != nil || string(clean) != "done" {
		t.Fatalf("a command that ended with 0 before the cancel: %q, %v", clean, err)
	}
}

// A recipe streamed to the container whose stream is cut short fails: a
// prefix of it never runs as if it were the whole.
func TestSandboxedToolNodeTruncatedStreamedRecipeFails(t *testing.T) {
	recipe := ": " + strings.Repeat("x", 150_000) + "\nexit 3\n"
	e, _ := sandboxedToolExecutor(t, "truncates")
	out, err := e.Execute(context.Background(), &ir.ToolNode{BaseNode: ir.BaseNode{ID: "big"}, Command: recipe}, map[string]any{})
	if err == nil {
		t.Fatalf("a recipe cut at 120,000 of its %d bytes succeeded: %v", len(recipe), out)
	}

	// Whole, the same streamed recipe runs to its own end.
	e, _ = sandboxedToolExecutor(t, "waits")
	out, err = e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "big-ok"},
		Command:  ": " + strings.Repeat("x", 150_000) + "\necho '{\"verdict\":\"pass\"}'\n",
	}, map[string]any{})
	if err != nil || out["verdict"] != "pass" {
		t.Fatalf("a whole streamed recipe did not run: %v, %v", err, out)
	}
	_, err = e.Execute(context.Background(), &ir.ToolNode{BaseNode: ir.BaseNode{ID: "big-fails"}, Command: recipe}, map[string]any{})
	if err == nil || errors.Is(err, sandbox.ErrExecStatusUnknown) {
		t.Fatalf("a whole streamed recipe ending in exit 3: %v, want its own status", err)
	}
}

// A proven end sweeps nothing: a command may leave a service running for the
// nodes after it, and it keeps running.
func TestSandboxedToolNodeProvenEndSweepsNothing(t *testing.T) {
	e, heart := sandboxedToolExecutor(t, "waits")
	out, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "starts-a-service"},
		Command: `( i=0; while [ "$i" -lt 30 ]; do printf 't\n' >> ` + shellQuoteForTest(heart) + `; i=$((i+1)); sleep 0.1; done ) >/dev/null 2>&1 &
echo '{"verdict":"pass"}'`,
	}, map[string]any{})
	if err != nil || out["verdict"] != "pass" {
		t.Fatalf("a command that started a service failed: %v, %v", err, out)
	}
	waitForHeartbeat(t, heart)
	before := fileSize(t, heart)
	time.Sleep(600 * time.Millisecond)
	if after := fileSize(t, heart); after <= before {
		t.Fatalf("the service a proven command left was stopped: heartbeat %d -> %d bytes", before, after)
	}
	waitHeartbeatStill(t, heart)
}

// waitHeartbeatStill waits for a bounded heartbeat loop to end on its own, so
// that the test's temp dir is not removed under it.
func waitHeartbeatStill(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		before := fileSize(t, path)
		time.Sleep(300 * time.Millisecond)
		if fileSize(t, path) == before {
			return
		}
	}
	t.Fatalf("the heartbeat at %s never stopped", path)
}

// An unknown end whose sweep could not finish is an ambiguous effect: the
// command may still be running, and the runtime must not start a second copy
// beside it. A sweep that finished leaves an ordinary failure.
func TestSandboxedToolNodeUnknownEndWithAFailedSweepIsAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sweepFails bool
		ambiguous  bool
	}{
		{"the sweep failed", true, true},
		{"the sweep finished", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireSetsid(t)
			dir := t.TempDir()
			heart := filepath.Join(dir, "heartbeat")
			e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
			e.sweepBudget = 200 * time.Millisecond
			e.SetSandbox(&execClientRun{client: "lost", dir: dir, running: heart, sweepFails: tc.sweepFails})
			// A short heartbeat: when the sweep fails nothing stops it, and it
			// ends on its own before the temp dir goes.
			_, err := e.Execute(context.Background(), &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "gate"},
				Command:  `i=0; while [ "$i" -lt 15 ]; do printf 't\n' >> ` + shellQuoteForTest(heart) + `; i=$((i+1)); sleep 0.1; done`,
			}, map[string]any{})
			var unknown *sandbox.ExecStatusUnknownError
			if !errors.As(err, &unknown) {
				t.Fatalf("a lost stream did not fail on its unknown end: %v", err)
			}
			if unknown.AmbiguousEffect() != tc.ambiguous {
				t.Fatalf("ambiguous = %v, want %v: %v", unknown.AmbiguousEffect(), tc.ambiguous, err)
			}
			if tc.sweepFails && strings.Contains(err.Error(), "found nothing left running") {
				t.Fatalf("a sweep that could not run claims it found nothing: %v", err)
			}
			if tc.sweepFails && !strings.Contains(err.Error(), "unable to upgrade connection") {
				t.Fatalf("the sweep's failure does not say why its exec failed: %v", err)
			}
			waitHeartbeatStill(t, heart)
		})
	}
}

// A Verified Action stops on an ambiguous end of its recipe: the recipe may
// still be running, and its postcondition cannot judge a world it may still
// be changing — a met postcondition beside a running recipe is no success.
func TestVerifiedToolNodeStopsOnAnAmbiguousRecipeEnd(t *testing.T) {
	requireSetsid(t)
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")
	marker := filepath.Join(dir, "applied.marker")
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	e.sweepBudget = 200 * time.Millisecond
	// The first postcondition runs through a healthy client, unmet; the
	// recipe's stream is then lost while the sweep's exec path is down; a
	// postcondition after it would be met.
	e.SetSandbox(&execClientRun{dir: dir, running: heart, sweepFails: true, clients: []string{"waits", "lost", "waits"}})
	out, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "applies"},
		Command:       `touch ` + shellQuoteForTest(marker) + `; i=0; while [ "$i" -lt 15 ]; do printf 't\n' >> ` + shellQuoteForTest(heart) + `; i=$((i+1)); sleep 0.1; done`,
		Postcondition: `test -f ` + shellQuoteForTest(marker),
	}, map[string]any{})
	if err == nil {
		t.Fatalf("a met postcondition beside a recipe that may still be running was a success: %v", out)
	}
	if !ambiguousEnd(err) {
		t.Fatalf("the ladder failed, but not on the recipe's ambiguous end: %v", err)
	}
	waitHeartbeatStill(t, heart)
}

// A client that never started ran nothing in the container: its own error is
// the failure, and there is no session to sweep.
func TestSandboxedToolNodeClientThatNeverStartedSweepsNothing(t *testing.T) {
	dir := t.TempDir()
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	run := &execClientRun{client: "unstartable", dir: dir}
	e.SetSandbox(run)
	_, err := e.Execute(context.Background(), &ir.ToolNode{BaseNode: ir.BaseNode{ID: "n"}, Command: "echo started"}, map[string]any{})
	if err == nil {
		t.Fatal("a client that never started succeeded")
	}
	if !strings.Contains(err.Error(), "no-such-exec-client") {
		t.Fatalf("the failure is not the client's own start failure — the command did not reach the sandbox: %v", err)
	}
	if errors.Is(err, sandbox.ErrExecStatusUnknown) {
		t.Fatalf("a client that never started was reported as an unproven end: %v", err)
	}
	if run.sweeps != 0 {
		t.Fatalf("%d sweep(s) for a command that never ran", run.sweeps)
	}
}

// A cancel that lands after a proven end sweeps nothing: the command ended,
// and what it left running is legitimately its own.
func TestSandboxedToolNodeCancelAfterAProvenEndSweepsNothing(t *testing.T) {
	dir := t.TempDir()
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	run := &execClientRun{client: "waits", dir: dir}
	e.SetSandbox(run)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := sandbox.NewCompletion("t")
	argv := c.Wrap([]string{"sh", "-c", "printf done"})
	stdout, _ := osexec.Command(argv[0], argv[1:]...).Output()
	clean, err := e.settleSandboxedRun(ctx, "n", c, stdout, nil)
	if err != nil || string(clean) != "done" {
		t.Fatalf("a proven end before the cancel: %q, %v", clean, err)
	}
	if run.sweeps != 0 {
		t.Fatalf("%d sweep(s) after a proven end", run.sweeps)
	}
}

// A postcondition whose own end is ambiguous is a failure, not a "not met":
// the recipe it would let run must not start beside a check that may still
// be running.
func TestVerifiedToolNodeStopsOnAnAmbiguousPostcondition(t *testing.T) {
	requireSetsid(t)
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")
	ran := filepath.Join(dir, "recipe.ran")
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	e.sweepBudget = 200 * time.Millisecond
	e.SetSandbox(&execClientRun{client: "waits", dir: dir, running: heart, sweepFails: true, clients: []string{"lost"}})
	_, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "checks"},
		Command:       `touch ` + shellQuoteForTest(ran),
		Postcondition: `i=0; while [ "$i" -lt 15 ]; do printf 't\n' >> ` + shellQuoteForTest(heart) + `; i=$((i+1)); sleep 0.1; done; false`,
	}, map[string]any{})
	if !ambiguousEnd(err) {
		t.Fatalf("a postcondition whose end is ambiguous did not stop the ladder: %v", err)
	}
	if _, serr := os.Stat(ran); serr == nil {
		t.Fatal("the recipe ran beside a postcondition that may still be running")
	}
	waitHeartbeatStill(t, heart)
}

// A sweep whose exec path is down a little longer than the stream is tried
// again: once it runs and stops the command, the end is an ordinary failure,
// which recovery may retry — never a run parked for a human because the first
// attempt fell in the same outage as the stream.
func TestSandboxedToolNodeSweepOutlivesAShortOutage(t *testing.T) {
	requireSetsid(t)
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	e.sweepBudget = 10 * time.Second
	run := &execClientRun{client: "lost", dir: dir, running: heart, sweepFailures: 1}
	e.SetSandbox(run)
	_, err := e.Execute(context.Background(), &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: grandchildRecipe(heart)}, map[string]any{})
	var unknown *sandbox.ExecStatusUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("a lost stream did not fail on its unknown end: %v", err)
	}
	if unknown.AmbiguousEffect() {
		t.Fatalf("a sweep that ran on its second attempt still left the end ambiguous: %v", err)
	}
	if run.sweeps < 2 {
		t.Fatalf("the sweep was tried %d time(s), want a second attempt after the first failed", run.sweeps)
	}
	assertGrandchildStopped(t, heart)
}

// A recipe streamed to the container means what `bash -s` makes of it — an
// unterminated heredoc runs to the end of the file, as bash reads one — and
// it keeps leading its own session, as an exec'd command does.
func TestSandboxedToolNodeStreamedRecipeMeansWhatBashMakesOfIt(t *testing.T) {
	e, _ := sandboxedToolExecutor(t, "waits")
	out, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "big"},
		Command: ": " + strings.Repeat("x", 150_000) + "\n" +
			`read -r pid comm state ppid pgrp sid rest < /proc/self/stat; [ "$pid" = "$sid" ] || echo NOT-LEADER` + "\n" +
			"cat <<EOF\nhello",
	}, map[string]any{})
	if err != nil {
		t.Fatalf("a streamed recipe bash runs failed: %v", err)
	}
	if got, _ := out["result"].(string); !strings.Contains(got, "hello") || strings.Contains(got, "NOT-LEADER") {
		t.Fatalf("the streamed recipe's output: %q, want its heredoc and its own session", got)
	}
}

// A Verified Action whose recipe's end was never proven succeeds on its
// postcondition alone: the partial output the recipe left is not its result.
func TestVerifiedToolNodeDropsThePartialOutputOfAnUnprovenRecipe(t *testing.T) {
	requireSetsid(t)
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")
	marker := filepath.Join(dir, "applied.marker")
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	e.SetSandbox(&execClientRun{dir: dir, running: heart, clients: []string{"waits", "cuts", "waits"}})
	out, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "applies"},
		Command:       `touch ` + shellQuoteForTest(marker) + `; echo '{"verdict":"pass"}'; i=0; while [ "$i" -lt 15 ]; do printf 't\n' >> ` + shellQuoteForTest(heart) + `; i=$((i+1)); sleep 0.1; done`,
		Postcondition: `test -f ` + shellQuoteForTest(marker),
	}, map[string]any{})
	if err != nil {
		t.Fatalf("a met postcondition after a swept recipe: %v", err)
	}
	if _, ok := out["verdict"]; ok {
		t.Fatalf("the partial output of a recipe whose end was never proven travelled: %v", out)
	}
	waitHeartbeatStill(t, heart)
}

// A cancelled Verified Action ends with its cancel — never with a best_effort
// success built on what the cancel cut short.
func TestVerifiedToolNodeCancelledEndsWithItsCancel(t *testing.T) {
	e, heart := sandboxedToolExecutor(t, "waits")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(ctx, &ir.ToolNode{
			BaseNode:      ir.BaseNode{ID: "va"},
			Command:       grandchildRecipe(heart),
			Postcondition: `test -f /nonexistent/never`,
			Policy:        ir.PolicyBestEffort,
		}, map[string]any{})
		done <- err
	}()
	waitForHeartbeat(t, heart)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled best_effort Verified Action succeeded")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled Verified Action did not return")
	}
	assertGrandchildStopped(t, heart)
}

// A cancel during the recipe runs nothing after it: no postcondition, no
// recovery rung starts on a context that is gone.
func TestVerifiedToolNodeRunsNothingAfterACancel(t *testing.T) {
	requireSetsid(t)
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	run := &execClientRun{client: "waits", dir: dir}
	e.SetSandbox(run)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(ctx, &ir.ToolNode{
			BaseNode:      ir.BaseNode{ID: "va"},
			Command:       grandchildRecipe(heart),
			Postcondition: `test -f /nonexistent/never`,
			Policy:        ir.PolicyBestEffort,
		}, map[string]any{})
		done <- err
	}()
	waitForHeartbeat(t, heart)
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled Verified Action did not return")
	}
	if run.tools != 2 {
		t.Fatalf("%d tool commands, want 2 (the first postcondition, the recipe): a rung ran after the cancel", run.tools)
	}
	assertGrandchildStopped(t, heart)
}

// A cancel during the postcondition that follows the recipe ends the node with
// its cancel, not with a best_effort success.
func TestVerifiedToolNodeCancelledDuringItsPostconditionEndsWithItsCancel(t *testing.T) {
	requireSetsid(t)
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")
	marker := filepath.Join(dir, "recipe.done")
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	e.SetSandbox(&execClientRun{client: "waits", dir: dir})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(ctx, &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "va"},
			Command:  `touch ` + shellQuoteForTest(marker),
			// Unmet at once before the recipe; long, then unmet, after it.
			Postcondition: `if [ -f ` + shellQuoteForTest(marker) + ` ]; then ` + grandchildRecipe(heart) + `; fi; false`,
			Policy:        ir.PolicyBestEffort,
		}, map[string]any{})
		done <- err
	}()
	waitForHeartbeat(t, heart)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a Verified Action cancelled during its postcondition succeeded under best_effort")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled Verified Action did not return")
	}
	assertGrandchildStopped(t, heart)
}

// A streamed recipe that reads its stdin reads its own next lines, as under
// `bash -s`: the line it reads is data, never a command.
func TestSandboxedToolNodeStreamedRecipeReadsItsNextLines(t *testing.T) {
	e, _ := sandboxedToolExecutor(t, "waits")
	out, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "big"},
		Command:  ": " + strings.Repeat("x", 150_000) + "\nread -r value\nexit 3\nprintf 'value=%s' \"$value\"\n",
	}, map[string]any{})
	if err != nil {
		t.Fatalf("the line a streamed recipe read ran as a command: %v", err)
	}
	if got, _ := out["result"].(string); got != "value=exit 3" {
		t.Fatalf("the streamed recipe's read: %q, want its next line as data", got)
	}
}

// A container without mktemp still runs a streamed recipe.
func TestSandboxedToolNodeStreamedRecipeWithoutMktemp(t *testing.T) {
	e, _ := sandboxedToolExecutor(t, "waits")
	bin := t.TempDir()
	for _, tool := range []string{"sh", "bash", "setsid", "cat", "wc", "rm", "sleep"} {
		p, err := osexec.LookPath(tool)
		if err != nil {
			t.Fatalf("%s is missing: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	e.sandbox.(*execClientRun).path = bin
	out, err := e.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "big"},
		Command:  ": " + strings.Repeat("x", 150_000) + "\nprintf healthy\n",
	}, map[string]any{})
	if err != nil || out["result"] != "healthy" {
		t.Fatalf("a streamed recipe in a container without mktemp: %v, %v", err, out)
	}
}
