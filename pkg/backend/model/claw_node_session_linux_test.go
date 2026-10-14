//go:build linux

package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// clawSessionWorld records the processes a test's node left and the
// background-work events the backend fired.
type clawSessionWorld struct {
	t      *testing.T
	pids   string
	mu     sync.Mutex
	events []delegate.BackgroundWork
}

func newClawSessionWorld(t *testing.T) *clawSessionWorld {
	t.Helper()
	w := &clawSessionWorld{t: t, pids: filepath.Join(t.TempDir(), "left.pids")}
	t.Setenv("NODE_SESSION_TEST_PIDS", w.pids)
	// A failing test must not leave its process behind: only the recorded
	// pids, and only while they still run `sleep 300`.
	t.Cleanup(func() {
		for _, pid := range w.left() {
			if cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && string(cmdline) == "sleep\x00300\x00" {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return w
}

func (w *clawSessionWorld) hooks() delegate.TaskHooks {
	return delegate.TaskHooks{OnBackgroundWork: func(bw delegate.BackgroundWork) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.events = append(w.events, bw)
	}}
}

func (w *clawSessionWorld) left() []int {
	raw, _ := os.ReadFile(w.pids)
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// check asserts that every process the node left is stopped and named by a
// claw abandoned event, and that the bystander runs.
func (w *clawSessionWorld) check(bystander int) {
	w.t.Helper()
	w.checkBackend(bystander, delegate.BackendClaw)
}

// checkBackend is check for a node delegated to backend.
func (w *clawSessionWorld) checkBackend(bystander int, backend string) {
	w.t.Helper()
	left := w.left()
	if len(left) == 0 {
		w.t.Fatal("scenario broken: the node recorded no process")
	}
	w.mu.Lock()
	events := slices.Clone(w.events)
	w.mu.Unlock()
	for _, pid := range left {
		if clawProcessRuns(pid) {
			w.t.Errorf("pid %d, left running by the node, outlived the node", pid)
		}
		if !slices.ContainsFunc(events, func(bw delegate.BackgroundWork) bool {
			return bw.Backend == backend && bw.Phase == delegate.BackgroundAbandoned &&
				slices.ContainsFunc(bw.Tasks, func(l string) bool { return strings.Contains(l, fmt.Sprintf("(pid %d)", pid)) })
		}) {
			w.t.Errorf("pid %d was stopped but no %s abandoned event named it: %+v", pid, backend, events)
		}
	}
	if !clawProcessRuns(bystander) {
		w.t.Errorf("a process the node did not start (pid %d) was stopped", bystander)
	}
}

func clawBystander(t *testing.T) int {
	t.Helper()
	other := exec.Command("sleep", "300")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	return other.Process.Pid
}

// What a claw node's shell starts ends with the node: the real bash builtin,
// driven by a scripted model through the real Execute, leaves a process
// running behind it, and Execute's end stops it. A bystander the test started
// itself survives.
func TestClawNodeShellLeavesNothingRunning(t *testing.T) {
	w := newClawSessionWorld(t)
	ws := t.TempDir()
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(tr, ws); err != nil {
		t.Fatal(err)
	}
	bash, err := tr.Resolve("bash")
	if err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf("nohup sleep 300 >/dev/null 2>&1 & echo $! >> %s", w.pids)
	input, _ := json.Marshal(map[string]any{"command": command})
	other := clawBystander(t)

	mock := newMockClient(toolUseEvents("tu_1", "bash", string(input), 10, 5), textEvents("done", 10, 5))
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	b := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	task := delegate.Task{
		NodeID: "agent", Model: "test/test-model", UserPrompt: "work", WorkDir: ws,
		HasTools: true, ToolMaxSteps: 5, Hooks: w.hooks(),
		ToolDefs: []delegate.ToolDef{{
			Name: "bash", Description: "shell",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
			Execute:     bash.Execute,
		}},
	}
	if _, err := b.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	w.check(other)
}

// clawLocalRun runs every command the backend sends to the sandbox on this
// machine — the runner's wrapper, the stand-in runner behind it, and the exec
// that ends the node — with the stand-in first on PATH, where the
// in-container `iterion` is.
type clawLocalRun struct {
	sandbox.Run
	path  string
	mu    sync.Mutex
	argvs [][]string
}

func (r *clawLocalRun) Driver() string { return "docker" }

func (r *clawLocalRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	r.mu.Lock()
	r.argvs = append(r.argvs, slices.Clone(argv))
	r.mu.Unlock()
	// The command is looked up the way the container looks it up: the
	// stand-ins first.
	name := argv[0]
	if !strings.Contains(name, "/") {
		if _, err := os.Stat(filepath.Join(r.path, name)); err == nil {
			name = filepath.Join(r.path, name)
		}
	}
	cmd := exec.CommandContext(ctx, name, argv[1:]...)
	env := os.Environ()
	for k, v := range opts.Env {
		if k != "PATH" {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = append(env, "PATH="+r.path+string(os.PathListSeparator)+os.Getenv("PATH"))
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}
	return cmd
}

// A sandboxed claw node's runner carries the node session: what the runner
// leaves running in the container ends with the node, through one exec that
// sweeps the runner's mark.
func TestClawSandboxedRunnerEndsWithTheNode(t *testing.T) {
	w := newClawSessionWorld(t)
	bin := t.TempDir()
	runner := `#!/bin/sh
[ "$1" = __claw-runner ] || exit 2
while read -r line; do
	case "$line" in *'"type":"task"'*) break ;; esac
done
nohup sleep 300 >/dev/null 2>&1 &
echo $! >> "$NODE_SESSION_TEST_PIDS"
printf '%s\n' '{"type":"result","data":{"output":{"text":"ok"},"backend_name":"claw"}}'
`
	if err := os.WriteFile(filepath.Join(bin, "iterion"), []byte(runner), 0o755); err != nil { // #nosec G306 — the stand-in must be executable
		t.Fatal(err)
	}
	other := clawBystander(t)
	run := &clawLocalRun{path: bin}
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	task := delegate.Task{NodeID: "agent", Model: "anthropic/test-model", UserPrompt: "work", WorkDir: t.TempDir(), Sandbox: run, Hooks: w.hooks()}
	if _, err := b.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	w.check(other)

	run.mu.Lock()
	argvs := slices.Clone(run.argvs)
	run.mu.Unlock()
	var mark string
	for _, argv := range argvs {
		if len(argv) > 4 && argv[0] == "sh" && slices.Equal(argv[len(argv)-2:], []string{"iterion", "__claw-runner"}) {
			i, j := strings.Index(argv[2], "/tmp/iterion-delegate-"), strings.Index(argv[2], ".pid")
			if i < 0 || j < i {
				t.Fatalf("the runner's wrapper names no pidfile: %s", argv[2])
			}
			mark = argv[2][i+len("/tmp/iterion-delegate-") : j]
			if !strings.Contains(argv[2], "export "+proc.NodeSessionVar(mark)+"=1 ") {
				t.Errorf("the runner's wrapper does not export its mark %q's node session variable: %s", mark, argv[2])
			}
		}
	}
	if mark == "" {
		t.Fatalf("the runner was not spawned through the pidfile wrapper: %q", argvs)
	}
	ends := 0
	for _, argv := range argvs {
		if len(argv) == 3 && strings.Contains(argv[2], "E='"+proc.NodeSessionEntry(mark)+"'") {
			ends++
		}
	}
	if ends != 1 {
		t.Errorf("%d exec(s) sweep the runner's mark, want exactly one", ends)
	}
}

func clawProcessRuns(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

// writeLeavingHook installs a workspace settings hook for event that leaves a
// process running behind it.
func writeLeavingHook(t *testing.T, ws, event string) {
	t.Helper()
	doc := map[string]any{"hooks": map[string]any{event: []any{map[string]any{
		"matcher": "",
		"hooks": []any{map[string]any{"type": "command",
			"command": `nohup sleep 300 >/dev/null 2>&1 & echo $! >> "$NODE_SESSION_TEST_PIDS"`}},
	}}}}
	raw, _ := json.Marshal(doc)
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// What a claw node's settings hooks start ends with the node, as it does when
// a claude_code CLI runs the same hooks as its own children — on a node with
// tools, and on a node with none, whose prompt still fires a hook.
func TestClawNodeHookLeavesNothingRunning(t *testing.T) {
	t.Run("a hook after a tool call", func(t *testing.T) {
		w := newClawSessionWorld(t)
		ws := t.TempDir()
		writeLeavingHook(t, ws, "PostToolUse")
		tr := tool.NewRegistry()
		if err := tool.RegisterClawBuiltins(tr, ws); err != nil {
			t.Fatal(err)
		}
		bash, err := tr.Resolve("bash")
		if err != nil {
			t.Fatal(err)
		}
		input, _ := json.Marshal(map[string]any{"command": "true"})
		other := clawBystander(t)
		mock := newMockClient(toolUseEvents("tu_1", "bash", string(input), 10, 5), textEvents("done", 10, 5))
		reg := NewRegistry()
		reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
		b := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
		task := delegate.Task{
			NodeID: "agent", Model: "test/test-model", UserPrompt: "work", WorkDir: ws,
			HasTools: true, ToolMaxSteps: 5, Hooks: w.hooks(),
			ToolDefs: []delegate.ToolDef{{
				Name: "bash", Description: "shell",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
				Execute:     bash.Execute,
			}},
		}
		if _, err := b.Execute(context.Background(), task); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		w.check(other)
	})
	t.Run("a node with no tool", func(t *testing.T) {
		w := newClawSessionWorld(t)
		ws := t.TempDir()
		writeLeavingHook(t, ws, "UserPromptSubmit")
		other := clawBystander(t)
		mock := newMockClient(textEvents("done", 10, 5))
		reg := NewRegistry()
		reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
		b := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
		task := delegate.Task{NodeID: "agent", Model: "test/test-model", UserPrompt: "work", WorkDir: ws, Hooks: w.hooks()}
		if _, err := b.Execute(context.Background(), task); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		w.check(other)
	})
}

// An LLM router delegated to a CLI agent is a delegation like any other: what
// its spawn leaves is stopped at its end, and the run hears it.
func TestLLMRouterSweepReachesTheRun(t *testing.T) {
	w := newClawSessionWorld(t)
	dir := t.TempDir()
	stand := filepath.Join(dir, "kimi")
	script := `#!/bin/sh
nohup sleep 300 >/dev/null 2>&1 &
echo $! >> "$NODE_SESSION_TEST_PIDS"
printf '%s\n' '{"role":"assistant","content":"{\"selected_route\":\"agent_a\",\"reasoning\":\"x\"}"}'
`
	if err := os.WriteFile(stand, []byte(script), 0o755); err != nil { // #nosec G306 — the stand-in must be executable
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	other := clawBystander(t)
	logger := iterlog.New(iterlog.LevelWarn, io.Discard)
	hooks := EventHooks{OnBackgroundWork: func(nodeID string, info BackgroundWorkInfo) {
		w.hooks().OnBackgroundWork(delegate.BackgroundWork{Backend: info.Backend, Phase: delegate.BackgroundPhase(info.Phase), Running: info.Running, Tasks: info.Tasks, Reason: info.Reason})
	}}
	reg := delegate.NewRegistry()
	reg.Register("kimi", delegate.NewKimiBackend(logger, stand))
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"sys": {Body: "system prompt"}}, Schemas: map[string]*ir.Schema{}}
	exec := NewClawExecutor(NewRegistry(), wf, WithBackendRegistry(reg), WithEventHooks(hooks),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: 10 * time.Millisecond}))
	node := &ir.RouterNode{
		BaseNode:   ir.BaseNode{ID: "fix_router"},
		LLMFields:  ir.LLMFields{Backend: "kimi", Model: "moonshot/kimi-k2", SystemPrompt: "sys"},
		RouterMode: ir.RouterLLM,
	}
	if _, err := exec.executeLLMRouterUnified(context.Background(), node, map[string]any{"_route_candidates": []string{"agent_a", "agent_b"}}); err != nil {
		t.Fatalf("router: %v", err)
	}
	w.checkBackend(other, "kimi")
}
