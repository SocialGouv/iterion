package delegate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// TestOpenCodeBuildArgs pins the whole invocation: the `run` subcommand
// closes argv, the prompt travels on stdin and NOT in argv, and the model /
// effort dials are the CLI's own.
func TestOpenCodeBuildArgs(t *testing.T) {
	b := &CLIAgentBackend{Protocol: opencodeProtocol}

	t.Run("full invocation", func(t *testing.T) {
		task := Task{
			UserPrompt:      "hello",
			SystemPrompt:    "be terse",
			Model:           "anthropic/claude-sonnet-4-6",
			ReasoningEffort: "high",
		}
		promptArg := task.BuildSystemPrompt() + "\n\n" + task.UserPrompt
		args, stdin := b.buildArgs(opencodeProtocol, task, promptArg, task.BuildSystemPrompt())

		want := []string{
			"--format", "json",
			"-m", "anthropic/claude-sonnet-4-6",
			"--variant", "high",
			"run",
		}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %v, want %v", args, want)
		}
		// The subcommand must be the LAST token: opencode reads everything
		// after it as the positional message.
		if args[len(args)-1] != "run" {
			t.Fatalf("subcommand is not last: %v", args)
		}
		if stdin != promptArg {
			t.Fatalf("stdin = %q, want the composed prompt %q", stdin, promptArg)
		}
		// A prompt in argv is what opencode parses as flags — the exact
		// failure this protocol avoids by using stdin.
		for _, a := range args {
			if strings.Contains(a, "hello") || strings.Contains(a, "be terse") {
				t.Fatalf("prompt leaked into argv: %v", args)
			}
		}
	})

	t.Run("a dash-leading prompt still travels whole", func(t *testing.T) {
		// opencode parses a positional "- bullet" as flags and prints its
		// help; on stdin the same text is the message.
		const prompt = "- a markdown bullet prompt"
		args, stdin := b.buildArgs(opencodeProtocol, Task{UserPrompt: prompt}, prompt, "")
		if stdin != prompt {
			t.Fatalf("stdin = %q, want %q", stdin, prompt)
		}
		for _, a := range args {
			if a == prompt {
				t.Fatalf("dash-leading prompt reached argv: %v", args)
			}
		}
	})

	t.Run("no dials when unset", func(t *testing.T) {
		args, _ := b.buildArgs(opencodeProtocol, Task{UserPrompt: "x"}, "x", "")
		for _, a := range args {
			if a == "-m" || a == "--variant" {
				t.Fatalf("unexpected dial with empty model/effort: %v", args)
			}
		}
	})
}

// TestOpenCodeProtocolLeavesSiblingsAlone guards the three protocols that
// existed before: adding opencode must not move their argv.
func TestOpenCodeProtocolLeavesSiblingsAlone(t *testing.T) {
	b := &CLIAgentBackend{Protocol: kimiProtocol}
	task := Task{UserPrompt: "hello", Model: "moonshot/kimi-k2"}
	args, stdin := b.buildArgs(kimiProtocol, task, "hello", "")
	want := []string{"-p", "hello", "--output-format", "stream-json", "-m", "moonshot/kimi-k2"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("kimi args moved: %v, want %v", args, want)
	}
	if stdin != "" {
		t.Fatalf("kimi stdin = %q, want empty", stdin)
	}
}

func TestOpenCodeMapEffort(t *testing.T) {
	cases := map[string][]string{
		"":          nil,
		"low":       {"--variant", "low"},
		"high":      {"--variant", "high"},
		"max":       {"--variant", "max"},
		"xhigh":     {"--variant", "high"},
		"ultracode": {"--variant", "high"},
		"HIGH":      {"--variant", "high"},
	}
	for in, want := range cases {
		if got := opencodeMapEffort(in); !reflect.DeepEqual(got, want) {
			t.Errorf("opencodeMapEffort(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestOpenCodeMapModel(t *testing.T) {
	// opencode's own -m format IS provider/model, so a spec passes through
	// whole: stripping the prefix (what grok does) would hand opencode a
	// bare id it resolves against the wrong provider.
	for in, want := range map[string]string{
		"anthropic/claude-sonnet-4-6": "anthropic/claude-sonnet-4-6",
		"  opencode/gpt-5-nano  ":     "opencode/gpt-5-nano",
		"gpt-5-nano":                  "gpt-5-nano",
		"":                            "",
	} {
		if got := opencodeMapModel(in); got != want {
			t.Errorf("opencodeMapModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseOpenCodeOutput walks the real event shape emitted by
// `opencode run --format json`.
func TestParseOpenCodeOutput(t *testing.T) {
	t.Run("text, session, tokens and cost", func(t *testing.T) {
		stream := strings.Join([]string{
			`{"type":"step_start","timestamp":1,"sessionID":"ses_abc","part":{"type":"step-start"}}`,
			`{"type":"text","timestamp":2,"sessionID":"ses_abc","part":{"type":"text","text":"hello "}}`,
			`{"type":"text","timestamp":3,"sessionID":"ses_abc","part":{"type":"text","text":"world"}}`,
			`{"type":"step_finish","timestamp":4,"sessionID":"ses_abc","part":{"type":"step-finish",` +
				`"tokens":{"total":180,"input":100,"output":50,"reasoning":20,"cache":{"read":7,"write":3}},"cost":0.0125}}`,
		}, "\n")

		got := parseOpenCodeOutput(stream)
		if got.Text != "hello world" {
			t.Errorf("Text = %q, want %q", got.Text, "hello world")
		}
		if got.SessionID != "ses_abc" {
			t.Errorf("SessionID = %q, want ses_abc", got.SessionID)
		}
		// opencode subtracts the cache halves from `input`; iterion reports
		// the real input, so they are added back.
		if got.InputTokens != 110 {
			t.Errorf("InputTokens = %d, want 110 (100 + 7 read + 3 write)", got.InputTokens)
		}
		// opencode subtracts reasoning from `output`; CLIAgentParse
		// documents thinking as a SUBSET of output, so it is added back.
		if got.OutputTokens != 70 {
			t.Errorf("OutputTokens = %d, want 70 (50 + 20 reasoning)", got.OutputTokens)
		}
		if got.ThinkingTokens != 20 {
			t.Errorf("ThinkingTokens = %d, want 20", got.ThinkingTokens)
		}
		if got.CostUSD != 0.0125 {
			t.Errorf("CostUSD = %v, want 0.0125", got.CostUSD)
		}
		if got.Err != nil {
			t.Errorf("Err = %v, want nil", got.Err)
		}
	})

	t.Run("steps accumulate", func(t *testing.T) {
		stream := strings.Join([]string{
			`{"type":"step_finish","sessionID":"s","part":{"tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.01}}`,
			`{"type":"step_finish","sessionID":"s","part":{"tokens":{"input":20,"output":7,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.02}}`,
		}, "\n")
		got := parseOpenCodeOutput(stream)
		if got.InputTokens != 30 || got.OutputTokens != 12 {
			t.Errorf("tokens = %d/%d, want 30/12", got.InputTokens, got.OutputTokens)
		}
		if got.CostUSD < 0.0299 || got.CostUSD > 0.0301 {
			t.Errorf("CostUSD = %v, want ~0.03", got.CostUSD)
		}
	})

	t.Run("an error event becomes a typed failure", func(t *testing.T) {
		stream := `{"type":"error","sessionID":"s","error":{"name":"ProviderAuthError","data":{"message":"no credential for anthropic"}}}`
		got := parseOpenCodeOutput(stream)
		if got.Err == nil {
			t.Fatal("Err = nil, want the stream's error")
		}
		if !strings.Contains(got.Err.Error(), "no credential for anthropic") {
			t.Errorf("Err = %v, want the nested message", got.Err)
		}
	})

	t.Run("error with no nested message falls back to the class name", func(t *testing.T) {
		stream := `{"type":"error","sessionID":"s","error":{"name":"UnknownError"}}`
		got := parseOpenCodeOutput(stream)
		if got.Err == nil || !strings.Contains(got.Err.Error(), "UnknownError") {
			t.Errorf("Err = %v, want UnknownError", got.Err)
		}
	})

	t.Run("unparseable stdout is handed back whole", func(t *testing.T) {
		got := parseOpenCodeOutput("not json at all")
		if got.Text != "not json at all" {
			t.Errorf("Text = %q, want the raw stream", got.Text)
		}
	})
}

// TestOpenCodeRefusesUntrustedProject is the security floor. Each case below
// was first PROVEN to execute code by running the real opencode CLI against
// the same layout with no credentials — the guard must refuse every one.
func TestOpenCodeRefusesUntrustedProject(t *testing.T) {
	// layout -> the files to create under the workspace root.
	vectors := map[string][]string{
		// module imported at startup
		"plugin dir":  {".opencode/plugin/pwn.ts"},
		"plugins dir": {".opencode/plugins/pwn.ts"},
		// same dynamic import, different directory name
		"tool dir":  {".opencode/tool/pwn.ts"},
		"tools dir": {".opencode/tools/pwn.ts"},
		// lifecycle scripts run during opencode's dependency install
		"package.json install hooks": {".opencode/package.json"},
		// a `plugin:` list in the root config — no .opencode/ at all
		"opencode.json":  {"opencode.json"},
		"opencode.jsonc": {"opencode.jsonc"},
		// anything at all under .opencode: enumerating the safe subset is the
		// game that never converges
		"an unfamiliar .opencode entry": {".opencode/whatever.ts"},
	}
	for name, files := range vectors {
		t.Run(name, func(t *testing.T) {
			ws := writeWorkspace(t, files...)
			err := refuseUntrustedOpenCodeProject(ws)
			if err == nil {
				t.Fatalf("%v accepted; opencode would load it as code", files)
			}
			for _, want := range []string{BackendOpenCode, openCodeTrustProjectEnv} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
			t.Setenv(openCodeTrustProjectEnv, "1")
			if err := refuseUntrustedOpenCodeProject(ws); err != nil {
				t.Errorf("%s=1 must opt into project trust, got %v", openCodeTrustProjectEnv, err)
			}
		})
	}

	// opencode reads resources from every level between the working directory
	// and the repository root, and a WorkDir strictly below BaseDir is a
	// supported shape (validateWorkDir permits it).
	t.Run("an ANCESTOR of the workdir, with and without a repository", func(t *testing.T) {
		for _, withGit := range []bool{false, true} {
			base := writeWorkspace(t, ".opencode/plugin/pwn.ts")
			if withGit {
				if err := os.MkdirAll(filepath.Join(base, ".git"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			work := filepath.Join(base, "sub", "pkg")
			if err := os.MkdirAll(work, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := refuseUntrustedOpenCodeProject(work); err == nil {
				t.Fatalf("git=%t: a plugin in an ancestor of the workdir was accepted", withGit)
			}
		}
	})

	// The walk must follow the path opencode's own process resolves. A
	// lexical walk over a symlinked workdir screens the directories the
	// symlink names and misses the ones the CLI actually reads.
	t.Run("a workdir reached through a SYMLINK", func(t *testing.T) {
		hostile := writeWorkspace(t, ".opencode/plugin/pwn.ts", ".git/HEAD")
		inner := filepath.Join(hostile, "inner")
		if err := os.MkdirAll(inner, 0o750); err != nil {
			t.Fatal(err)
		}
		clean := writeWorkspace(t, ".git/HEAD")
		link := filepath.Join(clean, "lnk")
		if err := os.Symlink(inner, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := refuseUntrustedOpenCodeProject(link); err == nil {
			t.Fatal("a symlinked workdir hid a hostile .opencode from the walk")
		}
	})

	// A cap that truncates a security walk fails OPEN.
	t.Run("a workspace too deep to screen is refused, not truncated", func(t *testing.T) {
		root := t.TempDir() // no .git anywhere, so nothing stops the climb
		deep := root
		for i := 0; i < openCodeMaxProjectLevels+4; i++ {
			deep = filepath.Join(deep, "d")
		}
		if err := os.MkdirAll(deep, 0o750); err != nil {
			t.Skipf("cannot build a deep tree here: %v", err)
		}
		err := refuseUntrustedOpenCodeProject(deep)
		if err == nil {
			t.Fatal("a workspace deeper than the cap was accepted without being screened")
		}
		for _, want := range []string{"cannot screen", "path components", "no repository root"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q — it must describe what was measured", err, want)
			}
		}
	})

	// ~/.opencode is the installer's own root, present on every host where
	// opencode exists. Screening HOME would refuse every run on such a host,
	// naming a directory the operator owns.
	t.Run("the operator's HOME is never screened", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".opencode", "bin"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".opencode", "bin", "opencode"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// A dotfiles HOME is a git repository on many hosts, which would
		// otherwise make HOME the walk's stop AND a screened level.
		if err := os.MkdirAll(filepath.Join(home, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
		work := filepath.Join(home, "projects", "app")
		if err := os.MkdirAll(work, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := refuseUntrustedOpenCodeProject(work); err != nil {
			t.Fatalf("refused on the operator's own home: %v", err)
		}
	})

	t.Run("the walk stops at the git worktree root", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		// Above the repository is the OPERATOR's tree, which opencode does
		// not read either (measured) — refusing on it would block every run
		// on a host whose home holds a .opencode directory.
		outer := t.TempDir()
		if err := os.MkdirAll(filepath.Join(outer, ".opencode", "plugin"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outer, ".opencode", "plugin", "p.ts"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		repo := filepath.Join(outer, "repo")
		work := filepath.Join(repo, "sub")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(work, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := refuseUntrustedOpenCodeProject(work); err != nil {
			t.Errorf("refused on a directory above the git root: %v", err)
		}
	})

	// The stop predicate must be no STRONGER than opencode's. A DANGLING
	// `.git` symlink exists to Lstat and not to Stat: it stopped this walk
	// while opencode climbed straight past it and loaded the parent's
	// plugin — measured end to end against the real CLI.
	t.Run("a DANGLING .git does not stop the walk", func(t *testing.T) {
		repo := writeWorkspace(t, ".opencode/plugin/pwn.ts", ".git/HEAD")
		work := filepath.Join(repo, "sub")
		if err := os.MkdirAll(work, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(repo, "nonexistent", "nowhere"), filepath.Join(work, ".git")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := refuseUntrustedOpenCodeProject(work); err == nil {
			t.Fatal("a dangling .git symlink truncated the walk; opencode does not stop there")
		}
	})

	// A RELATIVE workdir resolves against the server's cwd, which is where
	// runOnce's cmd.Dir would put the CLI. EvalSymlinks(".") succeeds and
	// returns "." unchanged, so resolving in the other order screens one
	// level of nothing and never reaches a single real ancestor.
	t.Run("a relative workdir is absolutised before the walk", func(t *testing.T) {
		levels, err := openCodeProjectLevels(".")
		if err != nil {
			t.Fatalf("openCodeProjectLevels(\".\") = %v", err)
		}
		if len(levels) == 0 || !filepath.IsAbs(levels[0]) {
			t.Fatalf("levels = %v, want absolute paths", levels)
		}
		if len(levels) < 2 {
			t.Fatalf("levels = %v, want the ancestors of the working directory", levels)
		}
	})

	// No workspace is unreachable from a real run, and neither "screen the
	// server's cwd" nor "screen nothing" would be right — a sandbox driver
	// defaults its --workdir to the bind-mounted checkout, so waving it
	// through would fail OPEN on exactly the directory at issue.
	t.Run("no workspace is refused, never waved through", func(t *testing.T) {
		if err := refuseUntrustedOpenCodeProject(""); err == nil {
			t.Error("an empty workspace was accepted without being screened")
		}
	})

	t.Run("a clean workspace runs", func(t *testing.T) {
		ws := t.TempDir()
		if err := refuseUntrustedOpenCodeProject(ws); err != nil {
			t.Errorf("clean workspace refused: %v", err)
		}
	})

	t.Run("an EMPTY .opencode directory is not a reason to refuse", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".opencode"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := refuseUntrustedOpenCodeProject(ws); err != nil {
			t.Errorf("empty .opencode refused: %v", err)
		}
	})

	t.Run("a level that cannot be screened is refused, never assumed clean", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads an unreadable directory")
		}
		ws := writeWorkspace(t, ".opencode/plugin/pwn.ts")
		dir := filepath.Join(ws, ".opencode")
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
		if err := refuseUntrustedOpenCodeProject(ws); err == nil {
			t.Fatal("an unreadable .opencode was read as clean")
		}
	})

	t.Run("the refusal fires through Execute, before any binary runs", func(t *testing.T) {
		ws := writeWorkspace(t, ".opencode/plugin/p.ts")
		b := NewOpenCodeBackend(nil, "/nonexistent/opencode")
		res, err := b.Execute(context.Background(), Task{WorkDir: ws, BaseDir: ws, UserPrompt: "hi"})
		if err == nil {
			t.Fatal("Execute accepted an untrusted workspace")
		}
		if !strings.Contains(err.Error(), openCodeTrustProjectEnv) {
			t.Errorf("Execute error %q does not name the escape hatch", err)
		}
		if res.BackendName != BackendOpenCode {
			t.Errorf("BackendName = %q, want %q", res.BackendName, BackendOpenCode)
		}
	})
}

// writeWorkspace materialises a temp workspace holding the given
// workspace-relative files.
func writeWorkspace(t *testing.T, files ...string) string {
	t.Helper()
	ws := t.TempDir()
	for _, rel := range files {
		path := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("export const x = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// TestOpenCodeResolveEnvPinsHostState: opencode folds the OPERATOR's
// ~/.claude into its prompt and aborts on a malformed skill there.
func TestOpenCodeResolveEnvPinsHostState(t *testing.T) {
	env := opencodeResolveEnv(context.Background())
	if env["OPENCODE_DISABLE_CLAUDE_CODE"] != "1" {
		t.Fatalf("OPENCODE_DISABLE_CLAUDE_CODE = %q, want 1 — iterion owns prompt composition", env["OPENCODE_DISABLE_CLAUDE_CODE"])
	}
}

// TestOpenCodeRefusesGatedNode: opencode declares no PermissionHook, so a
// node carrying an armed gate is refused rather than run ungated.
func TestOpenCodeRefusesGatedNode(t *testing.T) {
	policy, err := permission.NewPolicy(permission.ModeDeny, nil, nil, []string{"Bash"})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	b := NewOpenCodeBackend(nil, "")
	_, execErr := b.Execute(context.Background(), Task{
		WorkDir:    ws,
		BaseDir:    ws,
		UserPrompt: "hi",
		Permission: policy,
	})
	if execErr == nil {
		t.Fatal("a gated node ran on opencode; it cannot enforce the gate")
	}
	if !strings.Contains(execErr.Error(), BackendOpenCode) || !strings.Contains(execErr.Error(), "cannot enforce") {
		t.Fatalf("refusal %q must name the backend and the reason", execErr)
	}
}

// TestCLIAgentRefusesProtocolWithNoPromptDelivery: a protocol naming neither
// a prompt flag nor stdin delivery used to build argv with NO prompt and run
// the agent on an empty task.
func TestCLIAgentRefusesProtocolWithNoPromptDelivery(t *testing.T) {
	b := &CLIAgentBackend{Protocol: CLIAgentProtocol{Name: "mute", DefaultBinary: "true"}}
	_, err := b.Execute(context.Background(), Task{UserPrompt: "hi"})
	if err == nil {
		t.Fatal("a protocol that delivers no prompt was accepted")
	}
	if !strings.Contains(err.Error(), "delivers no prompt") {
		t.Fatalf("error = %v, want it to name the missing prompt delivery", err)
	}
}

// TestOpenCodeRegistered: the dispatch path the reproduction died on.
func TestOpenCodeRegistered(t *testing.T) {
	b, err := DefaultRegistry(nil).Resolve(BackendOpenCode)
	if err != nil {
		t.Fatalf("Resolve(%q) = %v, want a backend", BackendOpenCode, err)
	}
	if _, ok := b.(*OpenCodeBackend); !ok {
		t.Fatalf("Resolve(%q) returned %T, want *OpenCodeBackend", BackendOpenCode, b)
	}
	// The async capability is a type assertion, not a name list: opencode
	// must NOT claim it.
	if _, ok := b.(AsyncQuestionBackend); ok {
		t.Fatal("opencode claims AsyncQuestionBackend; it has no async question tools")
	}
}

// TestHostBinaryEnvRefusesRelativePath: runOnce sets cmd.Dir to the
// workspace and os/exec resolves a relative Path against Dir, so a relative
// override would run a binary out of the CHECKOUT — while detection resolved
// the same string against the server's own cwd.
func TestHostBinaryEnvRefusesRelativePath(t *testing.T) {
	b := &CLIAgentBackend{Protocol: opencodeProtocol}
	task := Task{WorkDir: t.TempDir()}

	t.Setenv("ITERION_OPENCODE_BIN", "./opencode")
	got, err := b.resolveBinary(task)
	if err == nil {
		t.Fatalf("resolveBinary = %q on a relative override; it would exec out of the workspace", got)
	}
	// The refusal names the variable and its value, so an operator can see
	// which string was rejected rather than watch a different binary run.
	for _, want := range []string{"ITERION_OPENCODE_BIN", "./opencode", "absolute"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}

	// Control: an absolute override is still honoured, so the assertion
	// above is not a test that cannot fail.
	abs := filepath.Join(t.TempDir(), "opencode")
	t.Setenv("ITERION_OPENCODE_BIN", abs)
	if got, err := b.resolveBinary(task); err != nil || got != abs {
		t.Fatalf("resolveBinary = (%q, %v), want the absolute override %q", got, err, abs)
	}

	// A BARE NAME has no checkout hazard: os/exec resolves it through PATH,
	// where cmd.Dir never enters. Refusing it would silently swap an
	// operator's explicit binary for another.
	t.Setenv("ITERION_OPENCODE_BIN", "opencode-nightly")
	if got, err := b.resolveBinary(task); err != nil || got != "opencode-nightly" {
		t.Fatalf("resolveBinary = (%q, %v), want the bare name honoured", got, err)
	}

	// Inside a sandbox a host path means nothing: the image supplies the CLI.
	t.Setenv("ITERION_OPENCODE_BIN", "./opencode")
	if got, err := b.resolveBinary(Task{WorkDir: task.WorkDir, Sandbox: stubSandboxRun{}}); err != nil || got != opencodeProtocol.DefaultBinary {
		t.Fatalf("sandboxed resolveBinary = (%q, %v), want the image's %q", got, err, opencodeProtocol.DefaultBinary)
	}
}

// TestPiRPCSharesTheBinaryChokepoint: pi drives two transports off the SAME
// variable, and RPC is the DEFAULT one. A rule applied to the print
// derivation only is a rule the default transport does not have — the
// relative override would still exec out of the checkout there.
func TestPiRPCSharesTheBinaryChokepoint(t *testing.T) {
	t.Setenv("ITERION_PI_BIN", "./bin/pi")
	ws := t.TempDir()

	_, printErr := (&CLIAgentBackend{Protocol: piProtocol}).resolveBinary(Task{WorkDir: ws})
	if printErr == nil {
		t.Fatal("the print transport accepted a relative override")
	}

	_, rpcErr := (&PiRPCBackend{}).Execute(context.Background(), Task{WorkDir: ws, BaseDir: ws, UserPrompt: "hi"})
	if rpcErr == nil {
		t.Fatal("the RPC transport accepted a relative override; it would exec out of the workspace")
	}
	if !strings.Contains(rpcErr.Error(), "ITERION_PI_BIN") {
		t.Fatalf("the RPC transport failed for another reason: %v", rpcErr)
	}
}
