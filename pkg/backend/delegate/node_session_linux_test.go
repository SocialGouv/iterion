//go:build linux

package delegate

import (
	"context"
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

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// These tests run the real Execute — and the formatting pass, and a CLI
// agent — against a stand-in that leaves a process running behind it, and
// check that the process ends with the node: on the host, and in a sandbox
// whose commands run on this machine. The processes stopped carry a token the
// engine drew for the test's own delegation; a bystander the test started
// itself must survive.

// fakeClaudeLeavingAProcess answers like the CLI after starting a process it
// never stops — a dev server started with nohup … &.
const fakeClaudeLeavingAProcess = `#!/bin/sh
case "$*" in *--input-format*)
	while read -r line; do
		case "$line" in *'"type":"user"'*) break ;; esac
	done ;;
esac
nohup sleep 300 >/dev/null 2>&1 &
echo $! >> "$NODE_SESSION_TEST_PIDS"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'
`

// detachedLeftover starts a process in a session of its own, out of reach of
// a kill of the stand-in's process group, and waits until it runs there: an
// SDK that kills the group when the stand-in exits would otherwise stop it
// first, and the test would pass without the session's end doing anything.
const detachedLeftover = `setsid nohup sleep 300 >/dev/null 2>&1 &
p=$!
echo $p >> "$NODE_SESSION_TEST_PIDS"
i=0
while [ $i -lt 500 ]; do
	set -- $(cat /proc/$p/stat 2>/dev/null)
	[ "$2" = "(sleep)" ] && [ "$6" = "$p" ] && break
	sleep 0.01; i=$((i+1))
done
`

// nodeSessionWorld is one test's stand-in, its record of what it left, and
// the warnings and background-work events the backend said.
type nodeSessionWorld struct {
	t      *testing.T
	dir    string
	pids   string
	mu     sync.Mutex
	warns  []string
	events []BackgroundWork
}

func newNodeSessionWorld(t *testing.T, script string) *nodeSessionWorld {
	t.Helper()
	w := &nodeSessionWorld{t: t, dir: t.TempDir()}
	w.pids = filepath.Join(w.dir, "left.pids")
	if err := os.WriteFile(filepath.Join(w.dir, "claude"), []byte(script), 0o755); err != nil { // #nosec G306 — the stand-in must be executable
		t.Fatal(err)
	}
	t.Setenv("NODE_SESSION_TEST_PIDS", w.pids)
	// A failing test must not leave its stand-in's process behind: only the
	// pids the stand-in recorded, and only while they still run `sleep 300`.
	t.Cleanup(func() {
		for _, pid := range w.left() {
			if cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && string(cmdline) == "sleep\x00300\x00" {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return w
}

func (w *nodeSessionWorld) stand() string { return filepath.Join(w.dir, "claude") }

func (w *nodeSessionWorld) logger() *iterlog.Logger {
	logger := iterlog.New(iterlog.LevelWarn, io.Discard)
	logger.SetHook(func(_ iterlog.Level, msg string, _ map[string]any) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.warns = append(w.warns, msg)
	})
	return logger
}

func (w *nodeSessionWorld) hooks() TaskHooks {
	return TaskHooks{OnBackgroundWork: func(bw BackgroundWork) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.events = append(w.events, bw)
	}}
}

func (w *nodeSessionWorld) left() []int {
	raw, _ := os.ReadFile(w.pids)
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// check asserts that every process the stand-in left is stopped and named by
// a background-work event of backend — and, when wantLabels, in the labels the
// delegation returned — that a warning named them, and that the bystander
// runs.
func (w *nodeSessionWorld) check(backend string, labels []string, wantLabels bool, bystander int) {
	w.t.Helper()
	left := w.left()
	if len(left) == 0 {
		w.t.Fatal("scenario broken: the stand-in recorded no process")
	}
	w.mu.Lock()
	warns, events := slices.Clone(w.warns), slices.Clone(w.events)
	w.mu.Unlock()
	for _, pid := range left {
		named := func(l string) bool { return strings.Contains(l, fmt.Sprintf("(pid %d)", pid)) }
		if processRuns(pid) {
			w.t.Errorf("pid %d, left running by the agent, outlived the node", pid)
		}
		if wantLabels && !slices.ContainsFunc(labels, named) {
			w.t.Errorf("pid %d was stopped but not in the labels: %q", pid, labels)
		}
		if !slices.ContainsFunc(events, func(bw BackgroundWork) bool {
			return bw.Backend == backend && bw.Phase == BackgroundAbandoned &&
				strings.HasPrefix(bw.Reason, nodeSessionEndReason) && slices.ContainsFunc(bw.Tasks, named)
		}) {
			w.t.Errorf("pid %d was stopped but no %s abandoned event named it: %+v", pid, backend, events)
		}
	}
	if !slices.ContainsFunc(warns, func(m string) bool { return strings.Contains(m, "node session: the node left") }) {
		w.t.Errorf("no warning named what the node left: %q", warns)
	}
	if !processRuns(bystander) {
		w.t.Errorf("a process the node did not start (pid %d) was stopped", bystander)
	}
}

// processRuns reports whether pid exists and has not exited (a zombie has).
func processRuns(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

// bystander starts a process no node started.
func bystander(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// forfaitCtx hands the delegation a forfait, as the runner does.
func forfaitCtx(t *testing.T) context.Context {
	t.Helper()
	resetClaudeCredEnv(t)
	forfait := t.TempDir()
	writeForfait(t, forfait, "at.before")
	ctx, cancel := context.WithTimeout(secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfait},
	}), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// localRun runs every command the backend sends to the sandbox on this
// machine — the spawn wrapper, the CLI behind it, and the exec that ends the
// spawn — with the stand-in first on PATH, where the in-container `claude` is.
type localRun struct {
	sandbox.Run
	path  string
	mu    sync.Mutex
	argvs [][]string
}

func (r *localRun) Driver() string { return "docker" }

func (r *localRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	r.mu.Lock()
	r.argvs = append(r.argvs, slices.Clone(argv))
	r.mu.Unlock()
	// The command is looked up the way the container looks it up: the
	// stand-in first.
	name := argv[0]
	if !strings.Contains(name, "/") {
		if _, err := os.Stat(filepath.Join(r.path, name)); err == nil {
			name = filepath.Join(r.path, name)
		}
	}
	cmd := exec.CommandContext(ctx, name, argv[1:]...)
	env := os.Environ()
	path := os.Getenv("PATH")
	for k, v := range opts.Env {
		if k == "PATH" {
			path = v
			continue
		}
		env = append(env, k+"="+v)
	}
	cmd.Env = append(env, "PATH="+r.path+string(os.PathListSeparator)+path)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}
	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	}
	return cmd
}

func TestExecuteStopsWhatTheAgentLeftOnTheHost(t *testing.T) {
	w := newNodeSessionWorld(t, fakeClaudeLeavingAProcess)
	ctx := forfaitCtx(t)
	other := bystander(t)
	b := &ClaudeCodeBackend{Logger: w.logger()}
	res, err := b.Execute(ctx, Task{NodeID: "agent", Command: w.stand(), WorkDir: t.TempDir(), UserPrompt: "work", Hooks: w.hooks()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	w.check(BackendClaudeCode, res.TerminatedBackgroundTasks, true, other)
}

func TestExecuteStopsWhatTheAgentLeftInTheSandbox(t *testing.T) {
	w := newNodeSessionWorld(t, fakeClaudeLeavingAProcess)
	ctx := forfaitCtx(t)
	other := bystander(t)
	run := &localRun{path: w.dir}
	b := &ClaudeCodeBackend{Logger: w.logger()}
	res, err := b.Execute(ctx, Task{NodeID: "agent", WorkDir: t.TempDir(), UserPrompt: "work", Sandbox: run, Hooks: w.hooks()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	w.check(BackendClaudeCode, res.TerminatedBackgroundTasks, true, other)
}

func TestFormattingPassStopsWhatItLeft(t *testing.T) {
	for _, sandboxed := range []bool{false, true} {
		t.Run(fmt.Sprintf("sandbox=%v", sandboxed), func(t *testing.T) {
			w := newNodeSessionWorld(t, fakeClaudeLeavingAProcess)
			ctx := forfaitCtx(t)
			other := bystander(t)
			task := Task{NodeID: "agent", WorkDir: t.TempDir(), OutputSchema: []byte(schemaOK), Hooks: w.hooks()}
			if sandboxed {
				task.Sandbox = &localRun{path: w.dir}
			} else {
				task.Command = w.stand()
			}
			b := &ClaudeCodeBackend{Logger: w.logger()}
			_, _, lost, _ := b.formatOutput(ctx, task, "s1")
			w.check(BackendClaudeCode, lost, true, other)
		})
	}
}

func TestCLIAgentStopsWhatTheAgentLeft(t *testing.T) {
	w := newNodeSessionWorld(t, `#!/bin/sh
nohup sleep 300 >/dev/null 2>&1 &
echo $! >> "$NODE_SESSION_TEST_PIDS"
printf '%s\n' '{"role":"assistant","content":"done"}'
`)
	isolateUserHome(t)
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	other := bystander(t)
	b := &CLIAgentBackend{Protocol: kimiProtocol, Command: w.stand(), Logger: w.logger()}
	if _, err := b.Execute(context.Background(), Task{NodeID: "agent", UserPrompt: "work", Model: "moonshot/kimi-k2", Hooks: w.hooks()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	w.check(kimiProtocol.Name, nil, false, other)
}

// A process the agent left holding its output does not hold the node: the
// agent's exit ends the wait for its output, and the process is stopped.
func TestCLIAgentReturnsWhileALeftoverHoldsItsOutput(t *testing.T) {
	w := newNodeSessionWorld(t, `#!/bin/sh
sleep 300 &
echo $! >> "$NODE_SESSION_TEST_PIDS"
printf '%s\n' '{"role":"assistant","content":"done"}'
`)
	isolateUserHome(t)
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	other := bystander(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b := &CLIAgentBackend{Protocol: kimiProtocol, Command: w.stand(), Logger: w.logger()}
	start := time.Now()
	if _, err := b.Execute(ctx, Task{NodeID: "agent", UserPrompt: "work", Model: "moonshot/kimi-k2", Hooks: w.hooks()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("Execute took %s: the node waited on the output a leftover held", took)
	}
	w.check(kimiProtocol.Name, nil, false, other)
}

func TestCodexStopsWhatTheAgentLeft(t *testing.T) {
	w := newNodeSessionWorld(t, `#!/bin/sh
case "$1" in --version) echo "codex-cli `+codexWebSearchMinCLIVersion+`"; exit 0;; esac
`+detachedLeftover+`exit 1
`)
	t.Setenv("CODEX_HOME", t.TempDir())
	other := bystander(t)
	b := &CodexBackend{Command: w.stand(), Logger: w.logger()}
	// The stand-in answers nothing: Execute fails, and its end still runs.
	// What it detached from the CLI's process group is the session's to stop.
	_, _ = b.Execute(context.Background(), Task{NodeID: "agent", UserPrompt: "work", WorkDir: t.TempDir(), Hooks: w.hooks()})
	w.check(BackendCodex, nil, false, other)
}

func TestPiStopsWhatTheAgentLeft(t *testing.T) {
	w := newNodeSessionWorld(t, `#!/bin/sh
`+detachedLeftover+`exit 1
`)
	other := bystander(t)
	work := t.TempDir()
	b := NewPiBackend(w.logger(), w.stand())
	// The stand-in answers nothing: the handshake fails, and the end of the
	// node still runs. What it detached from pi's process group is the
	// session's to stop.
	_, _ = b.Execute(context.Background(), Task{NodeID: "agent", WorkDir: work, BaseDir: work, UserPrompt: "work", Model: "openai/gpt-5.5", Hooks: w.hooks()})
	w.check(BackendPi, nil, false, other)
}

// Every sandboxed CLI spawn exports its mark as the node session token, and
// the exec that ends it sweeps exactly that mark.
func TestEverySandboxSpawnCarriesItsMarkAndIsSweptByIt(t *testing.T) {
	resetClaudeCredEnv(t)
	pinWatchdogs(t)
	for _, formatting := range []bool{false, true} {
		t.Run(fmt.Sprintf("formatting=%v", formatting), func(t *testing.T) {
			fake := &captureSandboxCmdRun{}
			task := Task{NodeID: "n", UserPrompt: "x", Sandbox: fake, OutputSchema: []byte(schemaOK)}
			b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
			if formatting {
				_, _, _, _ = b.formatOutput(context.Background(), task, "sid")
			} else {
				_, _ = b.Execute(context.Background(), task)
			}
			spawns, _, _ := fake.spawns()
			if len(spawns) == 0 {
				t.Fatal("scenario broken: no CLI spawn")
			}
			marks := map[string]bool{}
			for _, argv := range spawns {
				script := argv[2]
				i := strings.Index(script, "/tmp/iterion-delegate-")
				j := strings.Index(script, ".pid")
				if argv[0] != "sh" || i < 0 || j < i {
					t.Fatalf("the spawn is not wrapped by the pidfile writer: %v", argv[:3])
				}
				mark := script[i+len("/tmp/iterion-delegate-") : j]
				if !strings.Contains(script, "export "+proc.NodeSessionVar(mark)+"=1 ") {
					t.Errorf("the spawn does not export its mark %q's node session variable: %s", mark, script)
				}
				marks[mark] = true
			}
			for mark := range marks {
				ends := 0
				for i, argv := range fake.argvs {
					if !slices.Contains(argv, "--print") && len(argv) == 3 && strings.Contains(argv[2], "E='"+proc.NodeSessionEntry(mark)+"'") {
						ends++
						if len(fake.envs[i]) != 0 {
							t.Errorf("the end exec carries an environment: %v", fake.envs[i])
						}
					}
				}
				if ends != 1 {
					t.Errorf("mark %q: %d exec(s) sweep it, want exactly one", mark, ends)
				}
			}
		})
	}
}

// erringEndRun is a sandbox whose every exec answers as a sweep program that
// cannot look at the container's processes.
type erringEndRun struct{ sandbox.Run }

func (erringEndRun) Command(ctx context.Context, _ []string, _ sandbox.ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, "printf", "%s\n", "iterion-node-session error tr is missing in this container")
}

// A sweep that could not look, or did not finish, says so where the node's
// events are read — not only in the run log: a node whose leftovers no one
// could look at is not a clean node.
func TestASweepThatCannotLookSaysSoInTheEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(Task) []string
		want string
	}{
		{"in a sandbox", func(task Task) []string {
			_, end := SandboxNodeSession(erringEndRun{}, task, BackendClaw, iterlog.New(iterlog.LevelWarn, io.Discard))
			return end()
		}, "tr is missing"},
		{"here", func(task Task) []string {
			return SweepLocalNodeSession(iterlog.New(iterlog.LevelWarn, io.Discard), task, BackendClaw, "short", 0)
		}, "could not all be found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []BackgroundWork
			task := Task{NodeID: "agent", Hooks: TaskHooks{OnBackgroundWork: func(bw BackgroundWork) { got = append(got, bw) }}}
			if labels := tc.end(task); len(labels) != 0 {
				t.Errorf("a sweep that could not look reported stopping %q", labels)
			}
			if len(got) != 1 || got[0].Phase != BackgroundAbandoned || got[0].Backend != BackendClaw ||
				!strings.HasPrefix(got[0].Reason, nodeSessionUnsureReason) || !strings.Contains(got[0].Reason, tc.want) {
				t.Fatalf("events = %+v, want one claw abandoned event saying the sweep could not look (%q)", got, tc.want)
			}
		})
	}
}

// dockerishRun is localRun where, as with `docker exec`, the cmd the backend
// holds is a client: killing it — an abort, a timeout — leaves the
// in-container CLI running. The exec that ends the spawn runs as is.
type dockerishRun struct {
	localRun
	out string
}

func (r *dockerishRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	inner := r.localRun.Command(ctx, argv, opts)
	if len(argv) == 3 {
		return inner
	}
	client := exec.CommandContext(ctx, "sh", append([]string{"-c",
		`exec 3<&0; setsid "$@" <&3 3<&- >>"$DOCKERISH_OUT" 2>&1 & wait $!`, "client"}, inner.Args...)...)
	client.Env = append(inner.Env, "DOCKERISH_OUT="+r.out)
	client.Dir = inner.Dir
	client.Stdin = inner.Stdin
	return client
}

// A CLI still running when its node is aborted — whose client the abort
// killed, in a sandbox — has started a child with a cleared environment: the
// child is the session's through its parent, and the end finds it there
// before the CLI's own end orphans it. The CLI is the spawn, not something it
// left: it is not reported.
func TestTheEndOfAnAbortedSpawnFindsWhatItsCLIStarted(t *testing.T) {
	const leaves = `env -i /bin/sleep 300 &
echo $! >> "$NODE_SESSION_TEST_PIDS"
echo $$ > "$NODE_SESSION_TEST_PIDS.cli"
`
	for _, tc := range []struct {
		name    string
		script  string
		execute func(ctx context.Context, w *nodeSessionWorld, run sandbox.Run) ([]string, error)
		backend string
	}{
		{"cli agent", "#!/bin/sh\n" + leaves + "sleep 30\n",
			func(ctx context.Context, w *nodeSessionWorld, run sandbox.Run) ([]string, error) {
				isolateUserHome(t)
				t.Setenv("KIMI_CODE_HOME", t.TempDir())
				b := &CLIAgentBackend{Protocol: kimiProtocol, Command: w.stand(), Logger: w.logger()}
				_, err := b.Execute(ctx, Task{NodeID: "agent", UserPrompt: "work", Model: "moonshot/kimi-k2", Sandbox: run, Hooks: w.hooks()})
				return nil, err
			}, kimiProtocol.Name},
		{"claude_code", `#!/bin/sh
case "$*" in *--input-format*)
	while read -r line; do
		case "$line" in *'"type":"user"'*) break ;; esac
	done ;;
esac
` + leaves + `printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
sleep 30
`,
			func(ctx context.Context, w *nodeSessionWorld, run sandbox.Run) ([]string, error) {
				b := &ClaudeCodeBackend{Logger: w.logger()}
				res, err := b.Execute(ctx, Task{NodeID: "agent", WorkDir: t.TempDir(), UserPrompt: "work", Sandbox: run, Hooks: w.hooks()})
				return res.TerminatedBackgroundTasks, err
			}, BackendClaudeCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newNodeSessionWorld(t, tc.script)
			ctx := forfaitCtx(t)
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			other := bystander(t)
			run := &dockerishRun{localRun: localRun{path: w.dir}, out: filepath.Join(t.TempDir(), "out")}
			labels, _ := tc.execute(ctx, w, run)
			w.check(tc.backend, labels, false, other)
			raw, err := os.ReadFile(w.pids + ".cli")
			if err != nil {
				t.Fatalf("scenario broken: the CLI recorded no pid: %v", err)
			}
			cli := strings.TrimSpace(string(raw))
			w.mu.Lock()
			events := slices.Clone(w.events)
			w.mu.Unlock()
			for _, bw := range events {
				if slices.ContainsFunc(bw.Tasks, func(l string) bool { return strings.Contains(l, "(pid "+cli+")") }) {
					t.Errorf("the CLI itself (pid %s) is reported as something the node left: %+v", cli, bw)
				}
			}
			if slices.ContainsFunc(labels, func(l string) bool { return strings.Contains(l, "(pid "+cli+")") }) {
				t.Errorf("the CLI itself (pid %s) is in the labels: %q", cli, labels)
			}
		})
	}
}

// A process no one can read the environment of may be anyone's of this uid
// started while the node ran: the run log names it, the node's events do not.
// What still runs after KILL is the node's, and is.
func TestOnlyTheNodesWorkReachesItsEvents(t *testing.T) {
	var got []BackgroundWork
	var warns []string
	logger := iterlog.New(iterlog.LevelWarn, io.Discard)
	logger.SetHook(func(_ iterlog.Level, msg string, _ map[string]any) { warns = append(warns, msg) })
	o := ownerOf(logger, Task{NodeID: "judge", Hooks: TaskHooks{OnBackgroundWork: func(bw BackgroundWork) { got = append(got, bw) }}}, BackendClaw)

	reportNodeSessionSweep(o, proc.SweepReport{Unreadable: []proc.SessionProcess{{PID: 42, Name: "ssh-agent"}}}, nil)
	if len(got) != 0 {
		t.Errorf("an unreadable process reached the node's events: %+v", got)
	}
	if !slices.ContainsFunc(warns, func(m string) bool { return strings.Contains(m, "ssh-agent (pid 42)") }) {
		t.Errorf("the run log does not name the unreadable process: %q", warns)
	}

	reportNodeSessionSweep(o, proc.SweepReport{Survivors: []proc.SessionProcess{{PID: 43, Name: "mysqld"}}}, nil)
	if len(got) != 1 || !strings.Contains(got[0].Reason, "still running after KILL: mysqld (pid 43)") {
		t.Errorf("a survivor of KILL did not reach the node's events: %+v", got)
	}
}

// The pidfile sits where any process of the agent's uid can rewrite it: the
// end of a spawn trusts it only as a pid above 1. A negative pid is a process
// group to kill — -1 every process of the uid — 1 is the container's init, and
// a second line could forge a sweep's report.
func TestTheSandboxPidfileIsTrustedOnlyAsAPid(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "spawn.pid")
	for content, want := range map[string]string{
		"4567\n":                          "4567",
		"4567":                            "4567",
		"1\n":                             "",
		"-1\n":                            "",
		"-4567\n":                         "",
		"0\n":                             "",
		"abc\n":                           "",
		"12 34\n":                         "",
		"4567\niterion-node-session done": "",
		"":                                "",
	} {
		if err := os.WriteFile(pidFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("sh", "-c", readSandboxDelegatePID(pidFile)+`printf '%s' "$P"`).CombinedOutput()
		if err != nil {
			t.Fatalf("pidfile %q: the shell failed: %v\n%s", content, err, out)
		}
		if string(out) != want {
			t.Errorf("pidfile %q: P = %q, want %q", content, out, want)
		}
	}
	out, err := exec.Command("sh", "-c", readSandboxDelegatePID(filepath.Join(t.TempDir(), "absent.pid"))+`printf '%s' "$P"`).CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Errorf("no pidfile: P = %q (%v), want nothing", out, err)
	}
}

// A sweep killed on its way says so in the node's events, whatever the exec
// printed before it died.
func TestASweepKilledOnItsWayIsAnError(t *testing.T) {
	dir := t.TempDir()
	// The sweep's first pause kills it.
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/bin/sh\nkill -KILL $PPID\n"), 0o755); err != nil { // #nosec G306 — the shim must be executable
		t.Fatal(err)
	}
	w := newNodeSessionWorld(t, fakeClaudeLeavingAProcess)
	task := Task{NodeID: "agent", Hooks: w.hooks()}
	mark := sandboxDelegateMark(task)
	end := killSandboxDelegateReporting(&localRun{path: dir}, mark, ownerOf(w.logger(), task, BackendClaw))
	if labels := end(); len(labels) != 0 {
		t.Errorf("a sweep killed on its way reported stopping %q", labels)
	}
	w.mu.Lock()
	events := slices.Clone(w.events)
	w.mu.Unlock()
	if len(events) != 1 || !strings.HasPrefix(events[0].Reason, nodeSessionUnsureReason) {
		t.Fatalf("events = %+v, want one event saying the sweep did not finish", events)
	}
}

// In a sandbox whose exec is this machine's process (the noop driver), a
// process the agent left holding its output does not hold the node either.
func TestCLIAgentInANoopSandboxReturnsWhileALeftoverHoldsItsOutput(t *testing.T) {
	w := newNodeSessionWorld(t, `#!/bin/sh
sleep 300 &
echo $! >> "$NODE_SESSION_TEST_PIDS"
printf '%s\n' '{"role":"assistant","content":"done"}'
`)
	isolateUserHome(t)
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	d, err := noop.New()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{})
	if err != nil {
		t.Fatal(err)
	}
	other := bystander(t)
	b := &CLIAgentBackend{Protocol: kimiProtocol, Command: w.stand(), Logger: w.logger()}
	start := time.Now()
	if _, err := b.Execute(context.Background(), Task{NodeID: "agent", UserPrompt: "work", Model: "moonshot/kimi-k2", Hooks: w.hooks(), Sandbox: run}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("Execute took %s: the node waited on the output a leftover held", took)
	}
	w.check(kimiProtocol.Name, nil, false, other)
}

// The wrapper of a respawn kills the attempt the pidfile still records before
// it records itself: the previous attempt, a process this test started, ends.
func TestTheWrapperOfARespawnEndsThePreviousAttempt(t *testing.T) {
	mark := sandboxDelegateMark(Task{NodeID: "respawn"})
	pidFile := sandboxDelegatePIDFile(mark)
	t.Cleanup(func() { _ = os.Remove(pidFile); _ = os.Remove(sandboxDelegateSinceFile(mark)) })
	prev := exec.Command("sleep", "300")
	if err := prev.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prev.Process.Kill() })
	waited := make(chan error, 1)
	go func() { waited <- prev.Wait() }()
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(prev.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := wrapSandboxDelegateArgv(mark, []string{"true"})
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("the wrapper failed: %v\n%s", err, out)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the previous attempt still runs after the respawn's wrapper")
	}
}
