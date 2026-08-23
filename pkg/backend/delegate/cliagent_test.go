package delegate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/permissionhook"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// recordingRun is a minimal sandbox.Run that records the ExecOpts handed to
// Command and runs a canned host command in the container's stead, so a
// backend's sandbox wiring can be asserted without a real container.
// allOpts keeps every call in order: the agent invocation comes first,
// followed by lifecycle commands (the deferred pidfile-kill cleanup).
type recordingRun struct {
	gotOpts sandbox.ExecOpts // opts of the FIRST call — the agent invocation
	allOpts []sandbox.ExecOpts
	allArgv [][]string // argv of every call, so lifecycle commands are assertable
	script  string     // sh -c body the fake "container" runs
}

func (r *recordingRun) Driver() string { return "recording" }
func (r *recordingRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	if len(r.allOpts) == 0 {
		r.gotOpts = opts
	}
	r.allOpts = append(r.allOpts, opts)
	r.allArgv = append(r.allArgv, append([]string(nil), argv...))
	return exec.CommandContext(ctx, "sh", "-c", r.script) // #nosec G204 — test fixture
}
func (r *recordingRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (r *recordingRun) Cleanup(context.Context) error { return nil }

func testLogger() *iterlog.Logger { return iterlog.New(iterlog.LevelError, io.Discard) }

func TestCLIAgentBuildArgs(t *testing.T) {
	b := &CLIAgentBackend{Protocol: kimiProtocol, Logger: testLogger()}

	t.Run("kimi native argv, system folded into prompt", func(t *testing.T) {
		task := Task{
			SystemPrompt:     "be terse",
			SystemPromptMode: SystemPromptStandalone,
			UserPrompt:       "hello",
			Model:            "moonshot/kimi-k2",
		}
		promptArg := task.BuildSystemPrompt() + "\n\n" + task.UserPrompt
		args, stdin := b.buildArgs(kimiProtocol, task, promptArg, task.BuildSystemPrompt())
		want := []string{"-p", "be terse\n\nhello", "--output-format", "stream-json", "-m", "moonshot/kimi-k2"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %v, want %v", args, want)
		}
		if stdin != "" {
			t.Fatalf("stdin = %q, want empty (kimi passes prompt as -p arg)", stdin)
		}
	})

	t.Run("no model flag when model empty", func(t *testing.T) {
		args, _ := b.buildArgs(kimiProtocol, Task{UserPrompt: "x"}, "x", "")
		for _, a := range args {
			if a == "-m" {
				t.Fatalf("unexpected -m flag with empty model: %v", args)
			}
		}
	})

	t.Run("stdin delivery", func(t *testing.T) {
		proto := CLIAgentProtocol{Name: "t", DefaultBinary: "t", PromptFlag: "-", PromptViaStdin: true}
		args, stdin := (&CLIAgentBackend{Protocol: proto}).buildArgs(proto, Task{UserPrompt: "hi"}, "hi", "")
		if len(args) != 1 || args[0] != "-" {
			t.Fatalf("args = %v, want [-]", args)
		}
		if stdin != "hi" {
			t.Fatalf("stdin = %q, want hi", stdin)
		}
	})

	t.Run("effort + extra args", func(t *testing.T) {
		proto := CLIAgentProtocol{
			Name: "t", DefaultBinary: "t", PromptFlag: "-p",
			MapEffort: func(e string) []string { return []string{"--effort", e} },
			ExtraArgs: []string{"--yes"},
		}
		args, _ := (&CLIAgentBackend{Protocol: proto}).buildArgs(proto, Task{UserPrompt: "x", ReasoningEffort: "high"}, "x", "")
		want := []string{"-p", "x", "--effort", "high", "--yes"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %v, want %v", args, want)
		}
	})
}

func TestKimiMapModel(t *testing.T) {
	cases := map[string]string{
		"moonshot/kimi-k2": "moonshot/kimi-k2",
		"kimi/k2":          "kimi/k2",
		"kimi-k2":          "kimi-k2",
		"  moonshot/x  ":   "moonshot/x",
	}
	for in, want := range cases {
		if got := kimiMapModel(in); got != want {
			t.Errorf("kimiMapModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseStreamJSONText(t *testing.T) {
	t.Run("kimi 0.23 native role stream", func(t *testing.T) {
		stream := `{"role":"assistant","content":"final answer"}
{"role":"meta","type":"session.resume_hint","session_id":"sess-current","content":"resume"}`
		text, sid, tokens := parseStreamJSONText(stream)
		if text != "final answer" {
			t.Errorf("text = %q, want final answer", text)
		}
		if sid != "sess-current" {
			t.Errorf("sessionID = %q, want sess-current", sid)
		}
		if tokens != 0 {
			t.Errorf("tokens = %d, want 0 when stream reports no usage", tokens)
		}
	})

	t.Run("kimi native role stream keeps final assistant message", func(t *testing.T) {
		stream := `{"role":"assistant","content":"I will inspect the files first."}
{"role":"assistant","tool_calls":[{"type":"function","id":"tool-1"}]}
{"role":"tool","tool_call_id":"tool-1","content":"done"}
{"role":"assistant","content":"{\"answer\":\"42\"}"}
{"role":"meta","type":"session.resume_hint","session_id":"sess-tools"}`
		text, sid, _ := parseStreamJSONText(stream)
		if text != `{"answer":"42"}` {
			t.Errorf("text = %q, want final JSON message", text)
		}
		if sid != "sess-tools" {
			t.Errorf("sessionID = %q, want sess-tools", sid)
		}
	})

	t.Run("result event wins", func(t *testing.T) {
		stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}
{"type":"result","result":"final answer","session_id":"sess-1","usage":{"input_tokens":10,"output_tokens":5}}`
		text, sid, tokens := parseStreamJSONText(stream)
		if text != "final answer" {
			t.Errorf("text = %q, want %q", text, "final answer")
		}
		if sid != "sess-1" {
			t.Errorf("sessionID = %q, want sess-1", sid)
		}
		if tokens != 15 {
			t.Errorf("tokens = %d, want 15", tokens)
		}
	})

	t.Run("falls back to assistant text when no result", func(t *testing.T) {
		stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"part1 "}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"part2"}]}}`
		text, _, _ := parseStreamJSONText(stream)
		if text != "part1 part2" {
			t.Errorf("text = %q, want %q", text, "part1 part2")
		}
	})

	t.Run("non-json falls back to raw", func(t *testing.T) {
		text, _, _ := parseStreamJSONText("plain text output")
		if text != "plain text output" {
			t.Errorf("text = %q, want raw passthrough", text)
		}
	})
}

// TestCLIAgentExecute drives the backend end-to-end against a fake CLI script
// that echoes a stream-json result, verifying argv assembly, stdout parsing,
// and structured-output extraction.
func TestCLIAgentExecute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakekimi")
	// The script emits kimi-code 0.23's role stream whose content is a JSON object, so
	// the schema-aware fallback extracts it into Result.Output.
	script := `#!/bin/sh
printf '%s\n' '{"role":"assistant","content":"{\"answer\":\"42\"}"}'
printf '%s\n' '{"role":"meta","type":"session.resume_hint","session_id":"s9"}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { // #nosec G306 — test fixture must be executable
		t.Fatal(err)
	}

	b := &CLIAgentBackend{Protocol: kimiProtocol, Command: fake, Logger: testLogger()}
	task := Task{
		NodeID:       "n1",
		UserPrompt:   "what is the answer",
		Model:        "moonshot/kimi-k2",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
	}
	res, err := b.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.BackendName != BackendKimi {
		t.Errorf("BackendName = %q, want %q", res.BackendName, BackendKimi)
	}
	if res.SessionID != "s9" {
		t.Errorf("SessionID = %q, want s9", res.SessionID)
	}
	if res.ParseFallback {
		t.Errorf("ParseFallback = true, want false (result was valid JSON)")
	}
	if got := res.Output["answer"]; got != "42" {
		t.Errorf("Output[answer] = %v, want 42", got)
	}
	if res.Tokens != 0 {
		t.Errorf("Tokens = %d, want 0 when kimi reports no usage", res.Tokens)
	}
}

// TestCLIAgentSandboxStdin guards that a PromptViaStdin protocol running under
// a sandbox routes its prompt through sandbox.ExecOpts.Stdin (so the container
// driver allocates a forwarded stdin), rather than a post-hoc cmd.Stdin the
// driver silently drops.
func TestCLIAgentSandboxStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	proto := CLIAgentProtocol{
		Name:           "stdinbot",
		DefaultBinary:  "stdinbot",
		PromptViaStdin: true,
		ParseOutput:    parseStreamJSONText,
	}
	run := &recordingRun{script: `printf '%s\n' '{"type":"result","result":"ok"}'`}
	b := &CLIAgentBackend{Protocol: proto, Logger: testLogger()}
	_, err := b.Execute(context.Background(), Task{NodeID: "n1", UserPrompt: "the prompt", Sandbox: run})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if run.gotOpts.Stdin == nil {
		t.Fatal("ExecOpts.Stdin is nil — prompt would be dropped inside the container")
	}
	got, _ := io.ReadAll(run.gotOpts.Stdin)
	if string(got) != "the prompt" {
		t.Errorf("stdin = %q, want %q", got, "the prompt")
	}
}

func TestCLIAgentExecuteNoBinary(t *testing.T) {
	b := &CLIAgentBackend{Protocol: CLIAgentProtocol{Name: "x"}, Logger: testLogger()}
	_, err := b.Execute(context.Background(), Task{UserPrompt: "hi"})
	if err == nil {
		t.Fatal("expected error when no binary configured")
	}
}

func TestCLIAgentPermissionShadowHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hook command fixture")
	}
	binDir := t.TempDir()
	iterionBin := filepath.Join(binDir, "iterion")
	if err := os.WriteFile(iterionBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { // #nosec G306 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("ITERION_BIN", iterionBin)

	realHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(realHome, "config.toml"), []byte("model = \"k2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(realHome, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	policy, err := permission.NewPolicy(permission.ModeDeny, []string{"Read(**)"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{NodeID: "n", StoreDir: t.TempDir(), Permission: policy, ExtraEnv: []string{"KIMI_CODE_HOME=" + realHome}}
	env, cleanup, err := (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(context.Background(), task, kimiProtocol, BackendKimi)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || !strings.HasPrefix(env[0], "KIMI_CODE_HOME=") {
		t.Fatalf("hook env = %v", env)
	}
	shadow := strings.TrimPrefix(env[0], "KIMI_CODE_HOME=")
	config, err := os.ReadFile(filepath.Join(shadow, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"model = \"k2\"", "[[hooks]]", "PreToolUse", "__permission-hook", "--policy-b64", "cd " + shellQuote(shadow)} {
		if !strings.Contains(string(config), want) {
			t.Errorf("shadow config missing %q:\n%s", want, config)
		}
	}
	// The gate's authority must travel by value in the frozen argv, never as a
	// file the gated agent could rewrite between two tool calls (#498 R3e6bb0).
	wantPolicy, err := permissionhook.EncodePolicy(policy.Config())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), wantPolicy) {
		t.Errorf("registration does not carry the policy by value:\n%s", config)
	}
	if entries, err := os.ReadDir(shadow); err == nil {
		for _, e := range entries {
			if strings.Contains(e.Name(), "policy") {
				t.Errorf("a policy file was materialised in the shadow home: %s", e.Name())
			}
		}
	}
	// The shadow home must sit outside the workspace: <workspace>/.iterion is
	// exactly where a repo-scoped Edit(**)/Write(**) allow rule reaches.
	if stateRoot, _ := task.StateDir(BackendKimi); strings.HasPrefix(shadow, stateRoot) {
		t.Errorf("shadow home %s sits under the run state dir %s, which a repo-scoped write rule can reach", shadow, stateRoot)
	}
	if info, err := os.Lstat(filepath.Join(shadow, "credentials")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("credentials were not linked into the shadow home: info=%v err=%v", info, err)
	}
	cleanup()
	if _, err := os.Stat(shadow); !os.IsNotExist(err) {
		t.Errorf("shadow home survived cleanup: %v", err)
	}
	// Assert on `credentials`, not `config.toml`: config.toml is in
	// ExcludedEntries, so it is never symlinked and RemoveAll could not reach
	// it under any implementation — a vacuous assertion (#498 review R946e69).
	// `credentials` IS symlinked, so it is what proves cleanup unlinks rather
	// than follows.
	if info, err := os.Stat(filepath.Join(realHome, "credentials")); err != nil || !info.IsDir() {
		t.Errorf("cleanup followed the symlink into the operator home: info=%v err=%v", info, err)
	}
}

func TestCLIAgentPermissionUnsupportedModesRefuse(t *testing.T) {
	askPolicy, err := permission.NewPolicy(permission.ModeAsk, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(context.Background(), Task{Permission: askPolicy}, kimiProtocol, BackendKimi)
	if err == nil || !strings.Contains(err.Error(), "cannot pause") {
		t.Fatalf("permission: ask error = %v, want an explicit refusal", err)
	}

	denyWithAskRule, err := permission.NewPolicy(permission.ModeDeny, nil, []string{"Bash(git push:*)"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(context.Background(), Task{Permission: denyWithAskRule}, kimiProtocol, BackendKimi)
	if err == nil || !strings.Contains(err.Error(), "ask rules") {
		t.Fatalf("deny + ask rule error = %v, want an explicit refusal", err)
	}

	denyPolicy, err := permission.NewPolicy(permission.ModeDeny, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := &recordingRun{}
	_, _, err = (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(context.Background(), Task{Permission: denyPolicy, Sandbox: run}, kimiProtocol, BackendKimi)
	if err == nil || !strings.Contains(err.Error(), "sandboxed") {
		t.Fatalf("sandboxed gated run error = %v, want an explicit refusal", err)
	}

	// A noop-driver Run reads as Hostless() — but runOnce still takes the
	// `Sandbox != nil` branch, where ExecOpts.Env drops the shadow-home
	// override and the node would run ungated. The guard must key on the same
	// predicate runOnce does (#498 review, Re8c9e8).
	_, _, err = (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(context.Background(), Task{Permission: denyPolicy, Sandbox: noopLikeRun{}}, kimiProtocol, BackendKimi)
	if err == nil || !strings.Contains(err.Error(), "sandboxed") {
		t.Fatalf("noop-driver gated run error = %v, want the same refusal: ExecOpts.Env would drop the shadow home", err)
	}
}

// TestCLIAgentPermissionUnknownDialectRefuses pins the fail-CLOSED direction of
// the backendName → hook-dialect handshake (#498 review, Rd8d86a). A protocol
// that declares a PermissionHook under a name the hook subprocess cannot spell
// must be refused before the CLI is launched: the hook would exit non-zero with
// empty stdout, which grok and kimi both read as ALLOW, so the node would run
// completely ungated — the one outcome the gate exists to prevent.
func TestCLIAgentPermissionUnknownDialectRefuses(t *testing.T) {
	proto := kimiProtocol
	proto.Name = "some-future-cli"
	policy, err := permission.NewPolicy(permission.ModeDeny, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = (&CLIAgentBackend{Protocol: proto}).preparePermissionHook(
		context.Background(), Task{Permission: policy}, proto, proto.Name)
	if err == nil || !strings.Contains(err.Error(), "dialect") {
		t.Fatalf("unknown hook dialect error = %v, want an explicit refusal", err)
	}
}

// TestCLIAgentPermissionRefusesBinaryInsideWorkspace pins the third leg of the
// containment argument (#498 review, R8d24f4). The shadow home and the policy
// are both kept out of the agent's reach; the hook BINARY is the one the agent
// re-executes on every tool call, and a spawn failure is an ALLOW on both CLIs
// — so a corrupted file, not a crafted one, is enough to disarm the gate.
func TestCLIAgentPermissionRefusesBinaryInsideWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable-bit fixture")
	}
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "iterion")
	if err := os.WriteFile(inside, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { // #nosec G306 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("ITERION_BIN", inside)

	policy, err := permission.NewPolicy(permission.ModeDeny, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{NodeID: "n", WorkDir: workspace, StoreDir: t.TempDir(), Permission: policy}
	_, _, err = (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(
		context.Background(), task, kimiProtocol, BackendKimi)
	if err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("hook binary inside the gated workspace error = %v, want an explicit refusal", err)
	}
}

// TestCLIAgentPermissionRefusesRelativeBinInsideWorkspace pins R05d9fd: a
// relative ITERION_BIN (the shape CLAUDE.md documents as ITERION_BIN=./iterion)
// used to skip pathInsideCheckout — filepath.Rel cannot relate an absolute
// workspace to "./iterion", so the guard answered "outside" — and then freeze
// that relative path into the hook argv, which the CLI resolves against the
// gated workspace. Absolutising first makes the containment check see the
// real path and makes the spawn cwd-independent.
func TestCLIAgentPermissionRefusesRelativeBinInsideWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable-bit fixture")
	}
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "iterion")
	if err := os.WriteFile(inside, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { // #nosec G306 -- executable test fixture.
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("ITERION_BIN", "./iterion")

	policy, err := permission.NewPolicy(permission.ModeDeny, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{NodeID: "n", WorkDir: workspace, StoreDir: t.TempDir(), Permission: policy}
	_, _, err = (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(
		context.Background(), task, kimiProtocol, BackendKimi)
	if err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("relative ITERION_BIN inside the gated workspace error = %v, want an explicit refusal", err)
	}
}

// TestCLIAgentPermissionAbsolutisesRelativeBinOutsideWorkspace is the other
// half of R05d9fd: a relative ITERION_BIN that really is outside the workspace
// must still be frozen as an absolute path. Otherwise the CLI spawns it with
// cwd = the gated workspace and either hits a different file or fails open.
func TestCLIAgentPermissionAbsolutisesRelativeBinOutsideWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hook command fixture")
	}
	binDir := t.TempDir()
	workspace := t.TempDir()
	iterionBin := filepath.Join(binDir, "iterion")
	if err := os.WriteFile(iterionBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { // #nosec G306 -- executable test fixture.
		t.Fatal(err)
	}
	t.Chdir(binDir)
	t.Setenv("ITERION_BIN", "./iterion")

	realHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(realHome, "config.toml"), []byte("model = \"k2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := permission.NewPolicy(permission.ModeDeny, []string{"Read(**)"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{
		NodeID:     "n",
		WorkDir:    workspace,
		StoreDir:   t.TempDir(),
		Permission: policy,
		ExtraEnv:   []string{"KIMI_CODE_HOME=" + realHome},
	}
	env, cleanup, err := (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(
		context.Background(), task, kimiProtocol, BackendKimi)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if len(env) != 1 || !strings.HasPrefix(env[0], "KIMI_CODE_HOME=") {
		t.Fatalf("hook env = %v", env)
	}
	shadow := strings.TrimPrefix(env[0], "KIMI_CODE_HOME=")
	config, err := os.ReadFile(filepath.Join(shadow, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs("./iterion")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), abs) {
		t.Errorf("registration must carry the absolute hook binary %q:\n%s", abs, config)
	}
	if strings.Contains(string(config), "'./iterion'") {
		t.Errorf("registration still has a cwd-relative binary:\n%s", config)
	}
	if !strings.Contains(string(config), "cd "+shellQuote(shadow)) {
		t.Errorf("registration must cd out of the gated workspace:\n%s", config)
	}
}

// TestShellQuoteIsPOSIXNotCmdExe pins the quoting dialect that makes Windows
// ungatable: cmd.exe does not treat `'` as a quoting character, so this
// string would split on spaces and fail to spawn (#498 review, R1f6137).
func TestShellQuoteIsPOSIXNotCmdExe(t *testing.T) {
	got := shellQuote(`C:\Program Files\iterion\iterion.exe`)
	want := `'C:\Program Files\iterion\iterion.exe'`
	if got != want {
		t.Fatalf("shellQuote = %q, want POSIX single-quoting %q", got, want)
	}
}

// TestCLIAgentPermissionRefusesWindows pins R1f6137: the seam must not invite
// the operator past the symlink half (Developer Mode) into a POSIX-quoted
// hook command that cmd.exe cannot spawn — both CLIs read a spawn failure as
// ALLOW.
func TestCLIAgentPermissionRefusesWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only seam refusal")
	}
	policy, err := permission.NewPolicy(permission.ModeDeny, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = (&CLIAgentBackend{Protocol: kimiProtocol}).preparePermissionHook(
		context.Background(), Task{Permission: policy}, kimiProtocol, BackendKimi)
	if err == nil || !strings.Contains(err.Error(), "unsupported on Windows") {
		t.Fatalf("windows gated run error = %v, want an explicit refusal", err)
	}
}

func TestSameModelID(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"anthropic/claude-opus-5", "claude-opus-5", true},
		{"claude-opus-5", "anthropic/claude-opus-5", true},
		{"glm-4.6", "anthropic/claude-opus-5", false},
		{"", "", true},
		{"openai/gpt-5.5", "gpt-5.5", true},
		{"openai/gpt-5.5", "gpt-5.5-2026", true},
		{"gpt-5.5", "gpt-5.5-2026", true},
		{"gpt-5", "gpt-5.5", false},
		{"claude-sonnet-4", "claude-sonnet-4-6", false},
		{"kimi-k2", "kimi-k2-0905", true},
		{"gpt-4o", "gpt-4o-2024-08-06", true},
	}
	for _, tc := range cases {
		if got := SameModelID(tc.a, tc.b); got != tc.want {
			t.Errorf("SameModelID(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := SameModelID(tc.b, tc.a); got != tc.want {
			t.Errorf("SameModelID(%q, %q) = %v, want %v (symmetry)", tc.b, tc.a, got, tc.want)
		}
	}
}
