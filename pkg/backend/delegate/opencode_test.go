package delegate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestOpenCodeBuildArgs pins the whole invocation: the `run` subcommand OPENS
// argv, the prompt travels on stdin and NOT in argv, and the model / effort
// dials are the CLI's own, folded into one model reference.
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
			"run",
			"--format", "json",
			"-m", "anthropic/claude-sonnet-4-6#high",
		}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %v, want %v", args, want)
		}
		// The subcommand must be the FIRST token: opencode 2.x resolves it
		// before it knows any flag, and refuses a flag it reads first.
		if args[0] != "run" {
			t.Fatalf("subcommand is not first: %v", args)
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
			if a == "-m" || a == "--variant" || strings.Contains(a, "#") {
				t.Fatalf("unexpected dial with empty model/effort: %v", args)
			}
		}
	})

	t.Run("an effort with no model emits no model reference", func(t *testing.T) {
		// `-m "#high"` is a hard refusal ("Invalid model reference: #high",
		// measured on 2.2.5), so the dial is dropped — warned by name from
		// OpenCodeBackend.Execute, never spelled into argv.
		args, _ := b.buildArgs(opencodeProtocol, Task{UserPrompt: "x", ReasoningEffort: "high"}, "x", "")
		for _, a := range args {
			if a == "-m" || strings.Contains(a, "#") {
				t.Fatalf("an effort with no model reached argv: %v", args)
			}
		}
	})
}

// TestOpenCodeLegacyArgvDialect pins the ITERION_OPENCODE_ARGV=1 hatch: the
// 1.x argv, byte for byte — every generated flag first, `--variant` for the
// dial, `run` closing argv.
func TestOpenCodeLegacyArgvDialect(t *testing.T) {
	proto := opencodeLegacyProtocol()
	b := &CLIAgentBackend{Protocol: proto}
	task := Task{
		UserPrompt:      "hello",
		Model:           "anthropic/claude-sonnet-4-6",
		ReasoningEffort: "ultracode",
		SessionID:       "ses_1",
		ForkSession:     true,
	}
	args, stdin := b.buildArgs(proto, task, "hello", "")
	want := []string{
		"--format", "json",
		"-m", "anthropic/claude-sonnet-4-6",
		"--variant", "high",
		"--session", "ses_1", "--fork=true",
		"run",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("legacy args = %v, want %v", args, want)
	}
	if args[len(args)-1] != "run" {
		t.Fatalf("the 1.x subcommand is not last: %v", args)
	}
	if stdin != "hello" {
		t.Fatalf("stdin = %q, want the prompt", stdin)
	}
	// The dialect decides POSITION and spelling only: every other field is
	// the one the 2.x literal declares.
	if proto.Name != opencodeProtocol.Name || proto.OutputFormat != opencodeProtocol.OutputFormat ||
		!proto.PromptViaStdin || proto.HostBinaryEnv != opencodeProtocol.HostBinaryEnv {
		t.Fatalf("the legacy dialect changed more than the argv shape: %+v", proto)
	}
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
	// The 2.x dial is a bare variant name: it rides the model reference.
	cases := map[string]string{
		"":          "",
		"low":       "low",
		"high":      "high",
		"max":       "max",
		"xhigh":     "high",
		"ultracode": "high",
		"HIGH":      "high",
		"  high  ":  "high",
	}
	for in, want := range cases {
		if got := opencodeMapEffort(in); got != want {
			t.Errorf("opencodeMapEffort(%q) = %q, want %q", in, got, want)
		}
		if got := opencodeModelRef(Task{Model: "scripted/m1", ReasoningEffort: in}); got != "scripted/m1"+variantSuffix(want) {
			t.Errorf("opencodeModelRef(effort %q) = %q, want %q", in, got, "scripted/m1"+variantSuffix(want))
		}
		// The 1.x hatch spells the same level on its own flag.
		var legacy []string
		if want != "" {
			legacy = []string{"--variant", want}
		}
		if got := opencodeLegacyMapEffort(in); !reflect.DeepEqual(got, legacy) {
			t.Errorf("opencodeLegacyMapEffort(%q) = %v, want %v", in, got, legacy)
		}
	}
	// A reference that already spells its variant is emitted as written: a
	// second `#` is "Invalid model reference" on 2.2.5, and the operator's
	// explicit spelling outranks the dial (Execute warns —
	// TestOpenCodeAuthoredVariantOutranksTheDial).
	for _, effort := range []string{"", "high", "ultracode"} {
		if got := opencodeModelRef(Task{Model: "scripted/m1#low", ReasoningEffort: effort}); got != "scripted/m1#low" {
			t.Errorf("opencodeModelRef(authored #low, effort %q) = %q, want the authored reference unchanged", effort, got)
		}
	}
}

func variantSuffix(variant string) string {
	if variant == "" {
		return ""
	}
	return "#" + variant
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

	// The 2.x envelope carries `type` + `message` and neither `data` nor
	// `name`, so a reader of the 1.x keys alone reported no error at all.
	// Every line below is a verbatim 2.2.5 measurement.
	for _, tc := range []struct {
		name, stream, want string
	}{
		{
			name:   "a 2.x provider refusal carries its message",
			stream: `{"type":"error","timestamp":1793656111152,"sessionID":"ses_e61c3c152ffeiYZhwYVaPh5OHc","error":{"type":"provider.no-route","message":"Model unavailable: scripted/m1"}}`,
			want:   "Model unavailable: scripted/m1",
		},
		{
			name:   "a 2.x argv refusal carries its sentence",
			stream: `{"type":"error","timestamp":1,"sessionID":"","error":{"type":"unknown","message":"--fork requires --continue or --session"}}`,
			want:   "--fork requires --continue or --session",
		},
		{
			name:   "a 2.x error with no message falls back to its type",
			stream: `{"type":"error","sessionID":"s","error":{"type":"provider.no-route"}}`,
			want:   "provider.no-route",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOpenCodeOutput(tc.stream)
			if got.Err == nil {
				t.Fatalf("Err = nil, want %q", tc.want)
			}
			if !strings.Contains(got.Err.Error(), tc.want) {
				t.Errorf("Err = %v, want %q", got.Err, tc.want)
			}
		})
	}

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
			err := refuseUntrustedOpenCodeProject(ws, store.RunTrustDefault)
			if err == nil {
				t.Fatalf("%v accepted; opencode would load it as code", files)
			}
			for _, want := range []string{BackendOpenCode, openCodeTrustProjectEnv} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
			t.Setenv(openCodeTrustProjectEnv, "1")
			if err := refuseUntrustedOpenCodeProject(ws, store.RunTrustDefault); err != nil {
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
			if err := refuseUntrustedOpenCodeProject(work, store.RunTrustDefault); err == nil {
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
		if err := refuseUntrustedOpenCodeProject(link, store.RunTrustDefault); err == nil {
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
		err := refuseUntrustedOpenCodeProject(deep, store.RunTrustDefault)
		if err == nil {
			t.Fatal("a workspace deeper than the cap was accepted without being screened")
		}
		for _, want := range []string{"cannot screen", "path components", "filesystem root"} {
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
		if err := refuseUntrustedOpenCodeProject(work, store.RunTrustDefault); err != nil {
			t.Fatalf("refused on the operator's own home: %v", err)
		}
	})

	// A git worktree root does NOT stop the walk: opencode 2.2.5 climbs
	// straight past a `.git` and loads the ancestor's plugin (measured end
	// to end by TestLive_Feat_OpenCodeProjectScreen, whose `plugin above the
	// git root` row carries wantLoaded{2: true}). 1.1.19 stopped there, so
	// the walk takes the UNION of the two lines' reaches — a stop predicate
	// stronger than the CLI's is a hole. The operator lifts the extra
	// refusal with ITERION_OPENCODE_TRUST_PROJECT=1.
	t.Run("a resource ABOVE the git root is screened", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
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
		err := refuseUntrustedOpenCodeProject(work, store.RunTrustDefault)
		if err == nil {
			t.Fatal("a `.git` between the workspace and the plugin truncated the walk; opencode 2.x does not stop there")
		}
		if !strings.Contains(err.Error(), outer) {
			t.Errorf("error %q does not name the level it refused (%q)", err, outer)
		}
	})

	// A clean `.git` is not the only shape that used to truncate the walk: a
	// DANGLING `.git` symlink exists to Lstat and not to Stat, and a
	// predicate built on either reintroduces the hole above.
	t.Run("a DANGLING .git does not stop the walk", func(t *testing.T) {
		repo := writeWorkspace(t, ".opencode/plugin/pwn.ts", ".git/HEAD")
		work := filepath.Join(repo, "sub")
		if err := os.MkdirAll(work, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(repo, "nonexistent", "nowhere"), filepath.Join(work, ".git")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := refuseUntrustedOpenCodeProject(work, store.RunTrustDefault); err == nil {
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
		if err := refuseUntrustedOpenCodeProject("", store.RunTrustDefault); err == nil {
			t.Error("an empty workspace was accepted without being screened")
		}
	})

	t.Run("a clean workspace runs", func(t *testing.T) {
		ws := hermeticWorkspace(t)
		if err := refuseUntrustedOpenCodeProject(ws, store.RunTrustDefault); err != nil {
			t.Errorf("clean workspace refused: %v", err)
		}
	})

	t.Run("an EMPTY .opencode directory is not a reason to refuse", func(t *testing.T) {
		ws := hermeticWorkspace(t)
		if err := os.MkdirAll(filepath.Join(ws, ".opencode"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := refuseUntrustedOpenCodeProject(ws, store.RunTrustDefault); err != nil {
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
		if err := refuseUntrustedOpenCodeProject(ws, store.RunTrustDefault); err == nil {
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

// hermeticWorkspace is a fresh workspace whose screen reads nothing the test
// does not own: the operator's HOME is pinned to the workspace's parent, the
// one level where openCodeProjectLevels ends its walk without screening, so
// an opencode.json another process left in the runner's TMPDIR cannot redden
// a row. A `.git` at the workspace root would not do it — the production walk
// climbs past one, and nothing here narrows that.
// TestOpenCodeWitnessesReadNothingAboveTheirTempRoot holds every TestOpenCode*
// witness to this pin.
func hermeticWorkspace(t *testing.T) string {
	t.Helper()
	hermeticTempRoot(t)
	return t.TempDir()
}

// hermeticTempRoot pins the operator's HOME to the parent every t.TempDir()
// of this test shares, for a test whose workspace line is not its own to
// move: the walk from any of its temp dirs ends there.
func hermeticTempRoot(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", filepath.Dir(t.TempDir()))
}

// TestOpenCodeWitnessesReadNothingAboveTheirTempRoot holds the opencode
// family to its pin. Every TestOpenCode* test is re-run in a child of this
// binary whose TMPDIR sits under a root carrying both resource shapes the
// screen refuses. A witness whose workspace is a bare t.TempDir() climbs into
// that root and meets the untyped untrusted-checkout refusal instead of the
// outcome it asserts. A witness pinned by hermeticWorkspace or
// hermeticTempRoot stops below the root.
func TestOpenCodeWitnessesReadNothingAboveTheirTempRoot(t *testing.T) {
	// The trust hatch would wave every workspace through, the planted root
	// with it: parent and child both run with the screen armed.
	t.Setenv(openCodeTrustProjectEnv, "")
	root := t.TempDir()
	for _, rel := range []string{"opencode.json", ".opencode/plugin/pwn.ts"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(root, "tmp")
	probe := filepath.Join(tmp, "probe")
	if err := os.MkdirAll(probe, 0o750); err != nil {
		t.Fatal(err)
	}
	// The hazard is real: an unpinned walk from under the child's TMPDIR
	// refuses on the planted root.
	if err := refuseUntrustedOpenCodeProject(probe, store.RunTrustDefault); err == nil || !strings.Contains(err.Error(), root) {
		t.Fatalf("an unpinned walk from %s does not refuse on the planted root %s: %v", probe, root, err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenCode", "-test.skip=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v") // #nosec G204 — re-executes this test binary
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("an opencode witness reads above its own temp root — pin it with hermeticWorkspace or hermeticTempRoot: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestOpenCode") {
		t.Fatalf("the child ran no opencode witness:\n%s", out)
	}
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
// node carrying an armed gate is refused rather than run ungated — with the
// TYPED capability refusal, so recovery classifies it CAPABILITY_UNSUPPORTED
// (deterministic, never retried) instead of an untyped spawn-class error.
func TestOpenCodeRefusesGatedNode(t *testing.T) {
	hermeticTempRoot(t)
	// The witness is about the gate, not the image gap (#1643): the host
	// carries an opencode, so the answer is the same on a CI runner.
	stageHostCLI(t, "opencode")
	policy, err := permission.NewPolicy(permission.ModeDeny, nil, nil, []string{"Bash"})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	b := NewOpenCodeBackend(nil, "")
	_, execErr := b.Execute(context.Background(), Task{
		NodeID:     "n1",
		WorkDir:    ws,
		BaseDir:    ws,
		UserPrompt: "hi",
		Permission: policy,
	})
	if execErr == nil {
		t.Fatal("a gated node ran on opencode; it cannot enforce the gate")
	}
	var unsupported *ErrCapabilityUnsupported
	if !errors.As(execErr, &unsupported) {
		t.Fatalf("refusal %q (%T) must be the typed capability refusal", execErr, execErr)
	}
	if unsupported.Backend != BackendOpenCode || unsupported.NodeID != "n1" || unsupported.Capability != "permission-deny" {
		t.Fatalf("refusal = %+v, want backend %q, node n1, capability permission-deny", unsupported, BackendOpenCode)
	}
	if unsupported.Remedy == "" || !strings.Contains(execErr.Error(), "permission: off") {
		t.Fatalf("refusal %q must name the way out", execErr)
	}
	if !strings.Contains(unsupported.Remedy, "claude_code") {
		t.Fatalf("remedy %q must name an enforcing backend read from the registry — an empty registry would pass the non-empty check", unsupported.Remedy)
	}
}

// TestOpenCodeRefusesAskGatedNode: permission: ask on a hookless CLI hits the
// same typed refusal, named permission-ask — an ask verdict cannot pause an
// opencode run (headless auto-rejects, --auto auto-allows).
func TestOpenCodeRefusesAskGatedNode(t *testing.T) {
	stageHostCLI(t, "opencode")
	policy, err := permission.NewPolicy(permission.ModeAsk, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := NewOpenCodeBackend(nil, "")
	ws := hermeticWorkspace(t)
	_, execErr := b.Execute(context.Background(), Task{NodeID: "n1", WorkDir: ws, BaseDir: ws, UserPrompt: "hi", Permission: policy})
	var unsupported *ErrCapabilityUnsupported
	if !errors.As(execErr, &unsupported) {
		t.Fatalf("refusal %v (%T) must be the typed capability refusal", execErr, execErr)
	}
	if unsupported.Capability != "permission-ask" {
		t.Fatalf("capability = %q, want permission-ask", unsupported.Capability)
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
