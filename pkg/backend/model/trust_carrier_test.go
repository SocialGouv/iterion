package model

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The carrier bench, executor side (ADR-162 U1): the trust the engine pushes
// (SetWorkspaceTrust) reaches every task that names the workspace and every
// process the executor starts there. Each row has a trusted control: the
// trusted value is never written, so a trusted run's task and processes are
// what they were before the carrier existed.

// carriedTrust is the value a task or a process environment carries, by the
// last KEY=value entry (exec.Cmd keeps the last duplicate).
func carriedTrust(env []string) (string, bool) {
	val, found := "", false
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == envtrust.EnvRunTrust {
			val, found = v, true
		}
	}
	return val, found
}

func TestTrustCarrier_EveryTaskThatNamesTheWorkspaceCarriesTheTrust(t *testing.T) {
	for _, row := range []struct {
		name  string
		trust store.RunTrust
	}{
		{"trusted (the control)", store.RunTrustDefault},
		{"fork", store.RunTrustFork},
	} {
		t.Run(row.name, func(t *testing.T) {
			check := func(t *testing.T, surface string, task delegate.Task) {
				t.Helper()
				if task.WorkspaceTrust != row.trust {
					t.Errorf("%s task WorkspaceTrust = %q, want %q — the delegate would read the checkout as written by someone else", surface, task.WorkspaceTrust, row.trust)
				}
				got, found := carriedTrust(task.ExtraEnv)
				switch {
				case row.trust.Trusted() && found:
					t.Errorf("%s task env carries %s=%q on a trusted run — the trusted value is never written", surface, envtrust.EnvRunTrust, got)
				case !row.trust.Trusted() && got != string(row.trust):
					t.Errorf("%s task env %s = %q (present=%v), want %q — the delegate's processes would run as the tenant's own", surface, envtrust.EnvRunTrust, got, found, row.trust)
				}
			}

			t.Run("the agent task", func(t *testing.T) {
				be := &ledgerBackend{script: []ledgerStep{{res: delegate.Result{BackendName: delegate.BackendClaudeCode, Output: map[string]any{"ok": true}}}}}
				e := ledgerExecutor(be)
				e.SetWorkDir(t.TempDir())
				e.SetWorkspaceTrust(row.trust)
				if _, err := e.Execute(context.Background(), fallbackAgentNode("n", delegate.BackendClaudeCode, ""), map[string]any{}); err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if len(be.tasks) != 1 {
					t.Fatalf("the backend received %d tasks, want 1", len(be.tasks))
				}
				check(t, "agent", be.tasks[0])
			})

			t.Run("the router's task", func(t *testing.T) {
				be := &ledgerBackend{script: []ledgerStep{{res: delegate.Result{Output: map[string]any{"selected_route": "agent_a", "reasoning": "r"}}}}}
				e := newDelegateTestExecutor(be, EventHooks{})
				e.SetWorkDir(t.TempDir())
				e.SetWorkspaceTrust(row.trust)
				node := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, LLMFields: ir.LLMFields{Backend: "test_backend", SystemPrompt: "sys"}, RouterMode: ir.RouterLLM}
				if _, err := e.executeLLMRouterUnified(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_API_KEY": "test-funding"}), node, map[string]any{"_route_candidates": []string{"agent_a", "agent_b"}}); err != nil {
					t.Fatalf("router: %v", err)
				}
				if len(be.tasks) != 1 {
					t.Fatalf("the backend received %d tasks, want 1", len(be.tasks))
				}
				check(t, "router", be.tasks[0])
			})

			t.Run("the session packer's task", func(t *testing.T) {
				e := &ClawExecutor{}
				e.SetWorkDir(t.TempDir())
				e.SetWorkspaceTrust(row.trust)
				check(t, "packer", e.packerTask())
			})
		})
	}
}

// A tool node's command and script, on the host: real processes, read from
// what they print. An `iterion` such a script re-enters reads this variable.
func TestTrustCarrier_HostToolNodeProcessesCarryTheTrust(t *testing.T) {
	for _, row := range []struct {
		name  string
		trust store.RunTrust
		want  string
	}{
		{"trusted (the control)", store.RunTrustDefault, "<unset>"},
		{"fork", store.RunTrustFork, "fork"},
	} {
		t.Run(row.name, func(t *testing.T) {
			// A host value of the operator's own must not leak into the
			// verdict: the trusted control would read it.
			t.Setenv(envtrust.EnvRunTrust, "")
			if err := os.Unsetenv(envtrust.EnvRunTrust); err != nil {
				t.Fatal(err)
			}
			e := &ClawExecutor{}
			e.SetWorkDir(t.TempDir())
			e.SetWorkspaceTrust(row.trust)
			const probe = `printf '%s' "${ITERION_RUN_TRUST-<unset>}"`

			out, err := e.toolNodeCommand(context.Background(), probe, nil).cmd.Output()
			if err != nil {
				t.Fatalf("shell command: %v", err)
			}
			if string(out) != row.want {
				t.Errorf("a tool node's command saw %s=%q, want %q", envtrust.EnvRunTrust, out, row.want)
			}

			script := filepath.Join(t.TempDir(), "probe.sh")
			if err := os.WriteFile(script, []byte(probe+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err = e.toolNodeScriptCommand(context.Background(), []string{"sh"}, script, nil).cmd.Output()
			if err != nil {
				t.Fatalf("script command: %v", err)
			}
			if string(out) != row.want {
				t.Errorf("a tool node's script saw %s=%q, want %q", envtrust.EnvRunTrust, out, row.want)
			}
		})
	}
}

// The sandbox-routed twins of the same two builders, from the env map the
// sandbox is handed.
func TestTrustCarrier_SandboxedToolNodeCommandsCarryTheTrust(t *testing.T) {
	for _, row := range []struct {
		name  string
		trust store.RunTrust
	}{
		{"trusted (the control)", store.RunTrustDefault},
		{"fork", store.RunTrustFork},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := &ClawExecutor{}
			e.SetWorkspaceTrust(row.trust)
			fake := &isolatedSandboxRun{}
			e.SetSandbox(fake)
			// A node's own env block names the variable empty: the engine's
			// entry is applied over it, as the engine-child marker is.
			e.toolNodeCommand(context.Background(), "true", map[string]string{envtrust.EnvRunTrust: ""})
			e.toolNodeScriptCommand(context.Background(), []string{"sh"}, "probe.sh", nil)
			if len(fake.envs) != 2 {
				t.Fatalf("recorded %d commands, want 2", len(fake.envs))
			}
			for i, env := range fake.envs {
				got, found := env[envtrust.EnvRunTrust]
				switch {
				case row.trust.Trusted() && got != "":
					t.Errorf("sandboxed command %d carries %s=%q on a trusted run", i, envtrust.EnvRunTrust, got)
				case !row.trust.Trusted() && got != string(row.trust):
					t.Errorf("sandboxed command %d: %s = %q (present=%v), want %q", i, envtrust.EnvRunTrust, got, found, row.trust)
				}
			}
		})
	}
}

// trustStreamRun is a sandbox.Run whose `iterion __claw-runner` is a recorder:
// it keeps the env the runner was started with and every envelope line it
// reads, up to the task, then exits — nothing runs, the crossing is what is
// read. The mutex keeps the recorder race-free against the launcher's
// goroutines.
type trustStreamRun struct {
	rec string
	mu  sync.Mutex
	env map[string]string
}

func (r *trustStreamRun) Driver() string { return "docker" }

func (r *trustStreamRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	if isNodeSessionEnd(argv) {
		return exec.CommandContext(ctx, "true")
	}
	r.mu.Lock()
	r.env = maps.Clone(opts.Env)
	r.mu.Unlock()
	const script = `while IFS= read -r line; do printf '%s\n' "$line" >> "$1"; case "$line" in *'"type":"task"'*) exit 0;; esac; done`
	return exec.CommandContext(ctx, "sh", "-c", script, "sh", r.rec)
}

func (r *trustStreamRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, os.ErrInvalid
}

func (r *trustStreamRun) Cleanup(context.Context) error { return nil }

func (r *trustStreamRun) runnerEnv() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.env)
}

// The sandboxed claw path: the crossing carries the trust three ways — in the
// IOTask, in a workspace_trust envelope announced BEFORE the task (an older
// runner refuses the unknown envelope rather than rebuilding the task as
// trusted), and in the runner's own environment.
func TestTrustCarrier_TheSandboxCrossingCarriesTheTrust(t *testing.T) {
	for _, row := range []struct {
		name  string
		trust store.RunTrust
	}{
		{"trusted (the control)", store.RunTrustDefault},
		{"fork", store.RunTrustFork},
	} {
		t.Run(row.name, func(t *testing.T) {
			operatorEnv(t)
			t.Setenv("OPENAI_API_KEY", "sk-test-crossing")
			// ADR-162 U5: the fork lane crosses only a key OF THE RUN, as a
			// Layer 2 placeholder — the launcher's ambient env no longer
			// funds a crossing — so the row seals one to reach the stream.
			ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
				APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-run-crossing-witness"},
			})
			run := &trustStreamRun{rec: filepath.Join(t.TempDir(), "envelopes")}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, _ = newTestClawBackend().executeViaSandboxRunner(ctx, delegate.Task{
				NodeID: "n", Model: "openai/gpt-6", WorkDir: t.TempDir(), Sandbox: run, WorkspaceTrust: row.trust,
			})
			raw, err := os.ReadFile(run.rec)
			if err != nil {
				t.Fatalf("the runner read nothing: %v", err)
			}
			var announced *delegate.WorkspaceTrustData
			var task *delegate.IOTask
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var env delegate.Envelope
				if err := json.Unmarshal([]byte(line), &env); err != nil {
					t.Fatalf("decode envelope %q: %v", line, err)
				}
				switch env.Type {
				case delegate.EnvelopeWorkspaceTrust:
					if task != nil {
						t.Fatal("the workspace_trust envelope came AFTER the task — a runner that predates the field has already rebuilt it as trusted")
					}
					announced = &delegate.WorkspaceTrustData{}
					if err := json.Unmarshal(env.Data, announced); err != nil {
						t.Fatalf("decode workspace_trust: %v", err)
					}
				case delegate.EnvelopeTask:
					task = &delegate.IOTask{}
					if err := json.Unmarshal(env.Data, task); err != nil {
						t.Fatalf("decode task: %v", err)
					}
				}
			}
			if task == nil {
				t.Fatalf("no task envelope crossed — this row proves nothing:\n%s", raw)
			}
			if task.WorkspaceTrust != row.trust {
				t.Errorf("IOTask.WorkspaceTrust = %q, want %q — the runner rebuilds the task from the wire", task.WorkspaceTrust, row.trust)
			}
			if got := delegate.FromIOTask(*task).WorkspaceTrust; got != row.trust {
				t.Errorf("FromIOTask(...).WorkspaceTrust = %q, want %q", got, row.trust)
			}
			switch {
			case row.trust.Trusted() && announced != nil:
				t.Errorf("a trusted task announced trust %q — a mixed-version fleet must see no change for trusted runs", announced.WorkspaceTrust)
			case !row.trust.Trusted() && (announced == nil || announced.WorkspaceTrust != row.trust):
				t.Errorf("announced = %+v, want %q before the task", announced, row.trust)
			}
			got, found := run.runnerEnv()[envtrust.EnvRunTrust]
			switch {
			case row.trust.Trusted() && found:
				t.Errorf("a trusted runner env carries %s=%q", envtrust.EnvRunTrust, got)
			case !row.trust.Trusted() && got != string(row.trust):
				t.Errorf("runner env %s = %q (present=%v), want %q — the runner's main() would load the workspace .env, and what it starts would run trusted", envtrust.EnvRunTrust, got, found, row.trust)
			}
		})
	}
}
