package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// loggingRewriter is a compressor that records every command it is asked to
// rewrite — as rtk records every command it runs — and prefixes it.
func loggingRewriter(t *testing.T) (*rewrite.Chain, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "commands.log")
	bin := filepath.Join(dir, "fakerw")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$2\" >> '"+log+"'\nprintf 'echo compressed; %s' \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return rewrite.NewChain([]plugin.RewriterSpec{{ID: "fake", Locate: plugin.LocateSpec{Paths: []string{bin}},
		Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}, ApplyExitCodes: []int{0}}}}), log
}

// claw's agent loop with compression on: a command naming a secret runs with
// its value and never reaches the compressor; a command naming none does.
func TestAClawCommandNamingASecretIsNotCompressed(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	ws := t.TempDir()
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(tr, ws); err != nil {
		t.Fatal(err)
	}
	td, err := tr.Resolve("bash")
	if err != nil {
		t.Fatal(err)
	}
	chain, log := loggingRewriter(t)
	ctx := rewrite.WithChain(rewrite.WithMode(context.Background(), rewrite.On), chain)
	gt := &GenerationTool{Name: "bash", Execute: td.Execute}
	in, _ := json.Marshal(map[string]any{"command": "echo token=__ITERION_SECRET_GH_TOKEN__"})
	out, err := runToolExecution(ctx, gt, toolUseBlock{ID: "t1", Name: "bash", PartialJSON: string(in)}, g.Materialize, nil, nil)
	if err != nil || !strings.Contains(out, "token="+secret) {
		t.Fatalf("scenario broken: the command did not run with the value: %q %v", out, err)
	}
	if strings.Contains(out, "compressed") {
		t.Errorf("a command naming a secret was compressed: %q", out)
	}
	plain, _ := json.Marshal(map[string]any{"command": "echo plain"})
	if out, err := runToolExecution(ctx, gt, toolUseBlock{ID: "t2", Name: "bash", PartialJSON: string(plain)}, g.Materialize, nil, nil); err != nil || !strings.Contains(out, "compressed") {
		t.Fatalf("scenario broken: a command naming no secret was not compressed: %q %v", out, err)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), secret) || strings.Contains(string(b), "__ITERION_SECRET_") {
		t.Errorf("the compressor was handed the command naming a secret:\n%s", b)
	}
}

// A tool node that opts into compression: a command naming a secret runs
// uncompressed (the compressor would run it with the value), one naming none
// is compressed.
func TestAToolNodeCommandNamingASecretIsNotCompressed(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	chain, log := loggingRewriter(t)
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(t.TempDir()), WithSecretGuard(g), WithRewriteChain(chain))
	for _, c := range []struct {
		command    string
		compressed bool
	}{
		{"echo token=__ITERION_SECRET_GH_TOKEN__", false},
		{"echo plain", true},
	} {
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Command: c.command, Compress: "on"}
		out, err := exec.Execute(context.Background(), node, map[string]any{})
		if err != nil {
			t.Fatalf("%s: %v", c.command, err)
		}
		b, _ := json.Marshal(out)
		if got := strings.Contains(string(b), "compressed"); got != c.compressed {
			t.Errorf("%s: compressed = %v, want %v (%s)", c.command, got, c.compressed, b)
		}
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "__ITERION_SECRET_") || strings.Contains(string(b), secret) {
		t.Errorf("the compressor was handed the command naming a secret:\n%s", b)
	}
}

// A tool node naming its secret the way a .bot does ({{secrets.X}}): the
// resolved command carries the placeholder, the template does not — the
// command runs uncompressed with the value, and the compressor never sees it.
func TestAToolNodeSecretReferenceIsNotCompressed(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	chain, log := loggingRewriter(t)
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	ex := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(t.TempDir()), WithSecretGuard(g), WithRewriteChain(chain))
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Command: "echo token={{secrets.GH_TOKEN}}",
		CommandRefs: []*ir.Ref{{Kind: ir.RefSecrets, Path: []string{"GH_TOKEN"}, Raw: "{{secrets.GH_TOKEN}}"}}, Compress: "on"}
	out, err := ex.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), secret) {
		t.Fatalf("scenario broken: the command did not run with the value: %s", b)
	}
	if strings.Contains(string(b), "compressed") {
		t.Errorf("a {{secrets.X}} command was compressed: %s", b)
	}
	if lb, _ := os.ReadFile(log); strings.Contains(string(lb), "__ITERION_SECRET_") || strings.Contains(string(lb), secret) {
		t.Errorf("the compressor was handed the command naming a secret:\n%s", lb)
	}
}

// claw decodes the call's JSON: a placeholder escaped in the JSON text runs
// with the value — and does not reach the compressor either.
func TestAClawEscapedPlaceholderIsNotCompressed(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(tr, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	td, err := tr.Resolve("bash")
	if err != nil {
		t.Fatal(err)
	}
	chain, log := loggingRewriter(t)
	ctx := rewrite.WithChain(rewrite.WithMode(context.Background(), rewrite.On), chain)
	gt := &GenerationTool{Name: "bash", Execute: td.Execute}
	in := `{"command":"echo token=\u005f\u005fITERION_SECRET_GH_TOKEN\u005f\u005f"}`
	out, err := runToolExecution(ctx, gt, toolUseBlock{ID: "t1", Name: "bash", PartialJSON: in}, g.Materialize, nil, nil)
	if err != nil || !strings.Contains(out, "token="+secret) {
		t.Fatalf("scenario broken: %q %v", out, err)
	}
	if strings.Contains(out, "compressed") {
		t.Errorf("an escaped placeholder's command was compressed: %q", out)
	}
	if lb, _ := os.ReadFile(log); strings.Contains(string(lb), secret) {
		t.Errorf("the compressor was handed the value:\n%s", lb)
	}
}

// runEnvChain is a chain whose rewriter compresses nothing (its binary exits
// 1) and declares a run env turning its stores off, as rtk's does.
func runEnvChain(t *testing.T) []plugin.RewriterSpec {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakerw")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []plugin.RewriterSpec{{ID: "rtk", Locate: plugin.LocateSpec{Paths: []string{bin}},
		Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}},
		RunEnv: map[string]string{"RTK_DB_PATH": "/dev/null", "RTK_RECALL": "0"}}}
}

// A tool node's command runs with the chain's run env, over the inherited
// environment and the run's own, whatever its `compress:` (a command may run
// a rewriter itself); with no rewriter available, without.
func TestAToolNodesShellRunsWithTheRunEnvWhateverCompress(t *testing.T) {
	t.Setenv("RTK_DB_PATH", "/home/op/.local/share/rtk/history.db")
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	absent := []plugin.RewriterSpec{{ID: "rtk", Locate: plugin.LocateSpec{Paths: []string{filepath.Join(t.TempDir(), "missing")}},
		Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}}, RunEnv: map[string]string{"RTK_DB_PATH": "/dev/null", "RTK_RECALL": "0"}}}
	for _, c := range []struct {
		specs          []plugin.RewriterSpec
		compress, want string
	}{
		{runEnvChain(t), "on", "db=/dev/null recall=0"},
		{runEnvChain(t), "", "db=/dev/null recall=0"},
		{absent, "on", "db=/home/op/.local/share/rtk/history.db recall=1"},
	} {
		ex := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(t.TempDir()), WithRewriteChain(rewrite.NewChain(c.specs)))
		ex.SetRunExtraEnv([]string{"RTK_RECALL=1"})
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Command: `echo "db=$RTK_DB_PATH recall=$RTK_RECALL"`, Compress: c.compress}
		out, err := ex.Execute(context.Background(), node, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := json.Marshal(out); !strings.Contains(string(b), c.want) {
			t.Errorf("compress %q (%d specs): the command's environment: %s, want %s", c.compress, len(c.specs), b, c.want)
		}
	}
}

// Every command a tool node runs carries the run env — its recipe, a
// script: body, its postcondition — not its recipe only: each may run the
// rewriter itself.
func TestEveryCommandOfAToolNodeCarriesTheRunEnv(t *testing.T) {
	t.Setenv("RTK_DB_PATH", "/home/op/.local/share/rtk/history.db")
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	ex := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(t.TempDir()), WithRewriteChain(rewrite.NewChain(runEnvChain(t))))
	for _, node := range []*ir.ToolNode{
		{BaseNode: ir.BaseNode{ID: "sh"}, Script: `echo "db=$RTK_DB_PATH recall=$RTK_RECALL"`, Language: "sh"},
		{BaseNode: ir.BaseNode{ID: "bash"}, Script: `echo "db=$RTK_DB_PATH recall=$RTK_RECALL"`, Language: "bash"},
	} {
		out, err := ex.Execute(context.Background(), node, map[string]any{})
		if err != nil {
			t.Fatalf("%s: %v", node.ID, err)
		}
		if b, _ := json.Marshal(out); !strings.Contains(string(b), "db=/dev/null recall=0") {
			t.Errorf("%s script: its environment: %s", node.ID, b)
		}
	}
	// A postcondition met only under the run env: the recipe is skipped.
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "pc"}, Command: "touch recipe-ran",
		Postcondition: `test "$RTK_DB_PATH" = /dev/null && test "$RTK_RECALL" = 0`, Policy: ir.PolicyRequired}
	out, err := ex.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("postcondition: %v", err)
	}
	if m, _ := out["_verified_action"].(map[string]any); m["rung"] != "idempotent_skip" {
		t.Errorf("the postcondition ran without the run env: %v", out["_verified_action"])
	}
}

// envCaptureSandbox is a sandbox that records the environment each command is
// started with, and runs nothing.
type envCaptureSandbox struct{ envs []map[string]string }

func (f *envCaptureSandbox) Driver() string { return "env-capture" }
func (f *envCaptureSandbox) Command(ctx context.Context, _ []string, opts sandbox.ExecOpts) *osexec.Cmd {
	f.envs = append(f.envs, opts.Env)
	return osexec.CommandContext(ctx, "true")
}
func (f *envCaptureSandbox) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (f *envCaptureSandbox) Cleanup(context.Context) error { return nil }

// In a sandbox too, a tool node's script and shell commands start with the
// run env.
func TestASandboxedToolNodesCommandsCarryTheRunEnv(t *testing.T) {
	ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(t.TempDir()), WithRewriteChain(rewrite.NewChain(runEnvChain(t))))
	fake := &envCaptureSandbox{}
	ex.SetSandbox(fake)
	script := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "script"}, Script: "true", Language: "sh"}
	resolve, build := ex.scriptRecipe(context.Background(), script, map[string]any{})
	if _, cleanup, err := build(resolve()); err != nil {
		t.Fatal(err)
	} else if cleanup != nil {
		cleanup()
	}
	shell := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "shell"}, Command: "true"}
	resolve, build = ex.shellRecipe(context.Background(), shell, map[string]any{})
	if _, _, err := build(resolve()); err != nil {
		t.Fatal(err)
	}
	if len(fake.envs) != 2 {
		t.Fatalf("sandboxed commands: %d, want 2", len(fake.envs))
	}
	for i, env := range fake.envs {
		if env["RTK_DB_PATH"] != "/dev/null" || env["RTK_RECALL"] != "0" {
			t.Errorf("command %d started with %v, want the run env", i, env)
		}
	}
}

// A node's task carries the rewriters whatever its compression mode — its
// shell runs with their run env either way — and no mode when it does not
// compress; with no rewriter available, neither.
func TestATasksRewritersRideWhateverTheMode(t *testing.T) {
	node := &ir.AgentNode{}
	node.ID = "n"
	for _, c := range []struct {
		name, override, wantMode string
		chain                    *rewrite.Chain
		wantSpecs                int
	}{
		{"compressing", "", "on", rewrite.NewChain(runEnvChain(t)), 1},
		{"compression off", "off", "", rewrite.NewChain(runEnvChain(t)), 1},
		{"no rewriter", "", "", rewrite.NewChain(nil), 0},
	} {
		e := &ClawExecutor{logger: iterlog.Nop(), chain: c.chain, compressOverride: c.override}
		task, err := e.buildTask(context.Background(), node, backendFields{id: "n"}, map[string]any{}, delegate.BackendClaudeCode, nil)
		if err != nil {
			t.Fatalf("%s: buildTask: %v", c.name, err)
		}
		if task.CompressMode != c.wantMode || len(task.Rewriters) != c.wantSpecs {
			t.Errorf("%s: CompressMode %q, %d rewriters; want %q, %d", c.name, task.CompressMode, len(task.Rewriters), c.wantMode, c.wantSpecs)
		}
	}
}

// bashCallStream is one assistant turn calling bash with the command.
func bashCallStream(command string) <-chan api.StreamEvent {
	in, _ := json.Marshal(map[string]any{"command": command})
	ch := make(chan api.StreamEvent, 8)
	ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 10}
	ch <- api.StreamEvent{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_bash", Name: "bash"}}
	ch <- api.StreamEvent{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: string(in)}}
	ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
	ch <- api.StreamEvent{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 5}}
	ch <- api.StreamEvent{Type: api.EventMessageStop}
	close(ch)
	return ch
}

// claw's agent loop: its bash runs with the chain's run env, over the
// inherited environment and the run's own, whatever the mode (the agent may
// run a rewriter itself); with no rewriter, without.
func TestClawsShellRunsWithTheRunEnvWhateverTheMode(t *testing.T) {
	t.Setenv("RTK_DB_PATH", "/home/op/.local/share/rtk/history.db")
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(tr, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	td, err := tr.Resolve("bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		compress  string
		rewriters bool
		want      string
	}{
		{"on", true, "db=/dev/null recall=0"},
		{"off", true, "db=/dev/null recall=0"},
		{"on", false, "db=/home/op/.local/share/rtk/history.db recall=1"},
	} {
		var ran string
		bash := delegate.ToolDef{Name: "bash", Description: td.Description, InputSchema: td.InputSchema,
			Execute: func(ctx context.Context, in json.RawMessage) (string, error) {
				out, err := td.Execute(ctx, in)
				ran = out
				return out, err
			}}
		reg := NewRegistry()
		mock := &execMockClient{streams: []<-chan api.StreamEvent{
			bashCallStream(`echo "db=$RTK_DB_PATH recall=$RTK_RECALL"`),
			mockStreamEvents("done", "end_turn"),
		}}
		reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
		task := delegate.Task{NodeID: "agent", Model: "test/test-model", UserPrompt: "check", HasTools: true,
			ToolDefs: []delegate.ToolDef{bash}, ToolMaxSteps: 4, ExtraEnv: []string{"RTK_RECALL=1"}, CompressMode: c.compress}
		if c.rewriters {
			task.Rewriters = runEnvChain(t)
		}
		if _, err := NewClawBackend(reg, EventHooks{}, RetryPolicy{}).Execute(context.Background(), task); err != nil {
			t.Fatalf("compress %q: %v", c.compress, err)
		}
		if !strings.Contains(ran, c.want) {
			t.Errorf("compress %q (rewriters %v): bash ran with %q, want %s", c.compress, c.rewriters, ran, c.want)
		}
	}
}

// The real rtk, as the builtin declares it: the commands it compressed leave
// nothing in its stores — its history of the commands it ran, its recall of
// the lines a long output left out. Without the run env the same commands
// fill both (the probe sees rtk's stores). Skipped where rtk is not
// installed.
func TestTheBuiltinRtkKeepsNoStoreOfACompressedCommand(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	reg, err := plugin.Load()
	if err != nil {
		t.Fatal(err)
	}
	specs := reg.EnabledRewriterSpecs()
	if !rewrite.NewChain(specs).Available() {
		t.Skip("rtk is not installed")
	}
	ws := t.TempDir()
	var lines strings.Builder
	for i := range 60 {
		fmt.Fprintf(&lines, "line %d token=ghp_FAKEVALUE0123456789abcdef\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "settings.txt"), []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.InitRepo(t, ws)
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	for _, c := range []struct {
		name   string
		specs  []plugin.RewriterSpec
		stores []string
	}{
		{"builtin", specs, nil},
		{"without its run env", []plugin.RewriterSpec{func() plugin.RewriterSpec { s := specs[0]; s.RunEnv = nil; return s }()}, []string{"history.db", "recall.db"}},
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		for _, k := range []string{"XDG_DATA_HOME", "RTK_DB_PATH", "RTK_RECALL"} {
			t.Setenv(k, "") // restored at the end
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
		ex := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(ws), WithRewriteChain(rewrite.NewChain(c.specs)))
		for _, command := range []string{"git log --author=someone -1", "grep -rn token ."} {
			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Command: command, Compress: "on"}
			if _, err := ex.Execute(context.Background(), node, map[string]any{}); err != nil {
				t.Fatalf("%s: %s: %v", c.name, command, err)
			}
		}
		var kept []string
		if err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				kept = append(kept, strings.TrimPrefix(path, home))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if c.stores == nil && len(kept) > 0 {
			t.Errorf("%s: rtk kept %v", c.name, kept)
		}
		for _, store := range c.stores {
			if !slices.ContainsFunc(kept, func(k string) bool { return filepath.Base(k) == store }) {
				t.Errorf("%s: rtk kept %v, no %s: the probe does not see rtk's stores", c.name, kept, store)
			}
		}
	}
}
