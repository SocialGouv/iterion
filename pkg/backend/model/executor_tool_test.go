package model

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestExecutorToolNodeShellDenyAllPolicyRejects(t *testing.T) {
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
		WithToolPolicy(tool.DenyAllPolicy()),
		WithEventHooks(EventHooks{
			OnToolCall: func(nodeID string, info LLMToolCallInfo) {
				if nodeID != "shell_denied" {
					t.Errorf("nodeID = %q, want shell_denied", nodeID)
				}
				if info.ToolName != "shell:shell_denied" {
					t.Errorf("toolName = %q, want shell:shell_denied", info.ToolName)
				}
				if !errors.Is(info.Error, tool.ErrToolDenied) {
					t.Errorf("hook error = %v, want ErrToolDenied", info.Error)
				}
			},
		}),
	)

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "shell_denied"},
		Command:  "echo should-not-run",
	}
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	if !errors.Is(err, tool.ErrToolDenied) {
		t.Fatalf("err = %v, want ErrToolDenied", err)
	}
	if !strings.Contains(err.Error(), `tool "shell:shell_denied" denied`) {
		t.Fatalf("error lacks shell tool context: %v", err)
	}
}

func TestExecutorToolNodeScriptDenyAllPolicyRejects(t *testing.T) {
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
		WithToolPolicy(tool.DenyAllPolicy()),
		WithEventHooks(EventHooks{
			OnToolCall: func(nodeID string, info LLMToolCallInfo) {
				if nodeID != "script_denied" {
					t.Errorf("nodeID = %q, want script_denied", nodeID)
				}
				if info.ToolName != "script:sh:script_denied" {
					t.Errorf("toolName = %q, want script:sh:script_denied", info.ToolName)
				}
				if !errors.Is(info.Error, tool.ErrToolDenied) {
					t.Errorf("hook error = %v, want ErrToolDenied", info.Error)
				}
			},
		}),
	)

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "script_denied"},
		Script:   "echo should-not-run",
	}
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	if !errors.Is(err, tool.ErrToolDenied) {
		t.Fatalf("err = %v, want ErrToolDenied", err)
	}
	if !strings.Contains(err.Error(), `tool "script:sh:script_denied" denied`) {
		t.Fatalf("error lacks script tool context: %v", err)
	}
}

func TestExecutorToolNodeDirectPolicyAllows(t *testing.T) {
	t.Run("nil policy allows shell", func(t *testing.T) {
		exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{})
		node := &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "shell_allowed"},
			Command:  "echo ok",
		}
		out, err := exec.Execute(context.Background(), node, map[string]any{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if out["result"] != "ok" {
			t.Fatalf("result = %v, want ok", out["result"])
		}
	})

	t.Run("open policy allows script", func(t *testing.T) {
		exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()))
		node := &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "script_allowed"},
			Language: "sh",
			Script:   "echo ok",
		}
		out, err := exec.Execute(context.Background(), node, map[string]any{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if out["result"] != "ok" {
			t.Fatalf("result = %v, want ok", out["result"])
		}
	})
}

// executor_tool.go (carved out of executor.go in commit ab2fa26a) holds
// the tool-node helpers. End-to-end paths (executeToolNodeShell /
// executeToolNodeScript) need a ClawExecutor + workspace + sandbox
// scaffolding and are already exercised by e2e tests. The unit tests
// below pin the pure helpers — template substitution, env expansion,
// shell escaping, slice/map rendering, interpreter mapping, log
// formatting — which together account for the bulk of the file's
// logic and the highest regression risk if anyone touches them.

// ----- looksLikeShellCommand -----

func TestLooksLikeShellCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"bash", false},
		{"read_file", false},
		{"echo hello", true},            // space
		{"a|b", true},                   // pipe
		{"a&b", true},                   // background/and
		{"a;b", true},                   // semicolon
		{"a>b", true},                   // redirect
		{"a<b", true},                   // redirect
		{"a$VAR", true},                 // env-var
		{"`date`", true},                // backtick
		{"(echo)", true},                // subshell
		{"echo {}", true},               // brace
		{`echo "x"`, true},              // dquote
		{"echo 'x'", true},              // squote
		{"/usr/bin/echo", true},         // slash
		{"complex_tool_name123", false}, // bare identifier, no shell chars
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			if got := looksLikeShellCommand(c.cmd); got != c.want {
				t.Errorf("looksLikeShellCommand(%q) = %v, want %v", c.cmd, got, c.want)
			}
		})
	}
}

// ----- resolveCommandTemplate / resolveScriptTemplate / resolveTemplateWith -----

func ref(kind ir.RefKind, name, raw string, unquoted bool) *ir.Ref {
	return &ir.Ref{Kind: kind, Path: []string{name}, Raw: raw, Unquoted: unquoted}
}

func TestResolveCommandTemplate_BasicShellEscaping(t *testing.T) {
	tmpl := "echo {{input.msg}}"
	got := resolveCommandTemplate(tmpl, []*ir.Ref{ref(ir.RefInput, "msg", "{{input.msg}}", false)},
		map[string]any{"msg": "hello world"}, nil)
	if got != "echo 'hello world'" {
		t.Errorf("got %q", got)
	}
}

func TestResolveCommandTemplate_VarsLookup(t *testing.T) {
	tmpl := "echo {{vars.name}}"
	got := resolveCommandTemplate(tmpl, []*ir.Ref{ref(ir.RefVars, "name", "{{vars.name}}", false)},
		nil, map[string]any{"name": "Iterion"})
	if got != "echo 'Iterion'" {
		t.Errorf("got %q", got)
	}
}

func TestResolveCommandTemplate_RawBangBypassesShellEscape(t *testing.T) {
	tmpl := "{{!input.snippet}}"
	got := resolveCommandTemplate(tmpl, []*ir.Ref{ref(ir.RefInput, "snippet", "{{!input.snippet}}", true)},
		map[string]any{"snippet": "echo $HOME; ls"}, nil)
	// Raw form pastes verbatim — no shell-escaping.
	if got != "echo $HOME; ls" {
		t.Errorf("got %q", got)
	}
}

func TestResolveCommandTemplate_MissingValueLeftAsRaw(t *testing.T) {
	// substituteNil=false in shell context → unresolved refs stay literal.
	tmpl := "echo {{input.missing}}"
	got := resolveCommandTemplate(tmpl, []*ir.Ref{ref(ir.RefInput, "missing", "{{input.missing}}", false)},
		map[string]any{}, nil)
	if got != "echo {{input.missing}}" {
		t.Errorf("missing input should leave placeholder, got %q", got)
	}
}

func TestResolveRunRefs_CommandAndScript(t *testing.T) {
	const runID = "019ec1d3-f3b2-7a90-ae2f-bba543558786"
	runRef := []*ir.Ref{ref(ir.RefRun, "id", "{{run.id}}", false)}

	// Command context: {{run.id}} is not an input/vars/secrets ref, so the
	// shared resolver leaves it literal; resolveRunRefs must substitute it
	// (otherwise sec-audit-source's apply-mode branch ends up literally
	// named iterion/sec-fix/run.id after its sanitiser strips the braces).
	cmd := resolveRunRefs("git checkout -b iterion/sec-fix/{{run.id}}", runID, runRef, shellEscapeValue)
	if strings.Contains(cmd, "{{run.id}}") || !strings.Contains(cmd, runID) {
		t.Errorf("command run.id not substituted: %q", cmd)
	}

	// Script context: rendered as a JSON string literal so it parses.
	scr := resolveRunRefs("RUN_ID = {{run.id}}", runID, runRef, jsonLiteralValue)
	if !strings.Contains(scr, `"`+runID+`"`) {
		t.Errorf("script run.id should be a JSON string literal: %q", scr)
	}

	// Empty run id degrades to an empty rendering (the caller's sha
	// fallback then takes over) — never the literal placeholder.
	empty := resolveRunRefs("b/{{run.id}}", "", runRef, shellEscapeValue)
	if strings.Contains(empty, "{{run.id}}") {
		t.Errorf("empty run id should not leave the literal placeholder: %q", empty)
	}
}

func TestResolveScriptTemplate_NilBecomesJSONNull(t *testing.T) {
	// substituteNil=true in script context — nil renders as JSON "null".
	tmpl := "const v = {{input.missing}};"
	got := resolveScriptTemplate(tmpl, []*ir.Ref{ref(ir.RefInput, "missing", "{{input.missing}}", false)},
		map[string]any{"missing": nil}, nil)
	if got != "const v = null;" {
		t.Errorf("got %q", got)
	}
}

func TestResolveScriptTemplate_StringJSONQuoted(t *testing.T) {
	tmpl := "console.log({{input.s}});"
	got := resolveScriptTemplate(tmpl, []*ir.Ref{ref(ir.RefInput, "s", "{{input.s}}", false)},
		map[string]any{"s": `he said "hi"`}, nil)
	if got != `console.log("he said \"hi\"");` {
		t.Errorf("got %q", got)
	}
}

func TestResolveScriptTemplate_ObjectAndArray(t *testing.T) {
	tmpl := "const o = {{input.obj}}; const a = {{input.arr}};"
	got := resolveScriptTemplate(tmpl, []*ir.Ref{
		ref(ir.RefInput, "obj", "{{input.obj}}", false),
		ref(ir.RefInput, "arr", "{{input.arr}}", false),
	}, map[string]any{
		"obj": map[string]any{"k": "v"},
		"arr": []any{1.0, 2.0, 3.0},
	}, nil)
	if !strings.Contains(got, `{"k":"v"}`) || !strings.Contains(got, "[1,2,3]") {
		t.Errorf("got %q", got)
	}
}

func TestResolveTemplateWith_NoRefsPassthrough(t *testing.T) {
	got := resolveTemplateWith("plain text {no template}", nil, nil, nil, nil, shellEscapeValue, false)
	if got != "plain text {no template}" {
		t.Errorf("got %q", got)
	}
}

func TestResolveCommandTemplate_FileSecretPath(t *testing.T) {
	guard := secretguard.New([]secretguard.Secret{{
		Name:     "kubeconfig",
		Value:    "apiVersion: v1",
		FilePath: "/run/iterion/secrets/kubeconfig",
	}}, secretguard.DefaultConfig())
	got := resolveCommandTemplate("kubectl --kubeconfig {{secrets.kubeconfig.path}} get pods", []*ir.Ref{
		ref(ir.RefSecrets, "kubeconfig", "{{secrets.kubeconfig.path}}", false),
	}, nil, nil, guard)
	if got != "kubectl --kubeconfig '/run/iterion/secrets/kubeconfig' get pods" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveTemplateWith_NoCascadeReplay(t *testing.T) {
	// If input.a contains the literal "{{input.b}}", the value MUST NOT be
	// rewritten by a second pass — guards the cascade bug fixed by the
	// single-pass walk in resolveTemplateWith.
	tmpl := "X={{input.a}} Y={{input.b}}"
	got := resolveCommandTemplate(tmpl, []*ir.Ref{
		ref(ir.RefInput, "a", "{{input.a}}", true), // unquoted to pass raw
		ref(ir.RefInput, "b", "{{input.b}}", false),
	}, map[string]any{
		"a": "{{input.b}}", // literal that looks like a template
		"b": "shouldNotLeak",
	}, nil)
	// Expected: X gets the raw literal text; Y gets escaped 'shouldNotLeak'.
	if !strings.Contains(got, "X={{input.b}}") {
		t.Errorf("cascade replay corrupted input.a value: %q", got)
	}
	if !strings.Contains(got, "Y='shouldNotLeak'") {
		t.Errorf("input.b not substituted: %q", got)
	}
}

// ----- jsonLiteralValue / rawTemplateValue -----

func TestJSONLiteralValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "foo", `"foo"`},
		{"number", 42.0, "42"},
		{"bool true", true, "true"},
		{"nil", nil, "null"},
		{"slice", []any{1.0, 2.0}, "[1,2]"},
		{"map", map[string]any{"k": "v"}, `{"k":"v"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jsonLiteralValue(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestRawTemplateValue(t *testing.T) {
	if got := rawTemplateValue(nil); got != "null" {
		t.Errorf("nil → %q", got)
	}
	if got := rawTemplateValue("plain"); got != "plain" {
		t.Errorf("string → %q", got)
	}
	// Complex values delegate to formatValue (returns JSON-ish form);
	// we only care that the string survives unaltered for string input.
	if got := rawTemplateValue(map[string]any{"k": "v"}); !strings.Contains(got, "k") {
		t.Errorf("map didn't include key: %q", got)
	}
}

// ----- expandBracedEnv + helpers -----

func TestExpandBracedEnv_WithDefault(t *testing.T) {
	// :- form is always treated as shell ref even if env var is unset.
	got := expandBracedEnv("hello ${UNSET_VAR_XYZ:-world}")
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestExpandBracedEnv_FromActualEnv(t *testing.T) {
	t.Setenv("MY_VAR_FOR_TEST", "expanded")
	got := expandBracedEnv("hello ${MY_VAR_FOR_TEST}")
	if got != "hello expanded" {
		t.Errorf("got %q", got)
	}
}

func TestExpandBracedEnv_PreservesJSTemplateLiteral(t *testing.T) {
	// ${batchPackages.length} is not a valid env name → passthrough.
	got := expandBracedEnv("`size=${batchPackages.length}`")
	if got != "`size=${batchPackages.length}`" {
		t.Errorf("JS template literal eaten: %q", got)
	}
}

func TestExpandBracedEnv_PreservesUnsetWithoutDefault(t *testing.T) {
	// Unset env var, no `:-default` → preserve verbatim so the downstream
	// language sees what the author wrote.
	got := expandBracedEnv("hello ${DEFINITELY_NOT_SET_4242}")
	if got != "hello ${DEFINITELY_NOT_SET_4242}" {
		t.Errorf("got %q", got)
	}
}

func TestExpandBracedEnv_UnterminatedBracePassthrough(t *testing.T) {
	got := expandBracedEnv("hello ${FOO")
	if got != "hello ${FOO" {
		t.Errorf("got %q", got)
	}
}

func TestBracedEnvWouldExpand(t *testing.T) {
	// Explicit default → always expandable.
	if !bracedEnvWouldExpand("X:-fallback") {
		t.Error("with default should be expandable")
	}
	// No default, env unset → not expandable.
	if bracedEnvWouldExpand("DEFINITELY_NOT_SET_4242") {
		t.Error("unset without default should not be expandable")
	}
	// Env set → expandable.
	t.Setenv("MY_SET_VAR", "x")
	if !bracedEnvWouldExpand("MY_SET_VAR") {
		t.Error("set var should be expandable")
	}
}

func TestLooksLikeEnvRef(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"", false},
		{"FOO", true},
		{"foo_bar", true},
		{"_FOO123", true},
		{"FOO:-default", true},
		{"FOO:-anything goes here", true},
		{"foo.bar", false}, // dot disqualifies
		{"foo bar", false}, // space disqualifies
		{"123FOO", false},  // leading digit
		{"foo(", false},    // paren
		{"foo[0]", false},  // bracket
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			if got := looksLikeEnvRef(c.body); got != c.want {
				t.Errorf("looksLikeEnvRef(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestResolveBracedEnvBody_WithDefault(t *testing.T) {
	got := resolveBracedEnvBody("UNSET_VAR_FOR_TEST_XYZ:-fallback")
	if got != "fallback" {
		t.Errorf("got %q", got)
	}
}

func TestResolveBracedEnvBody_EnvWins(t *testing.T) {
	t.Setenv("PRESET_VAR", "from-env")
	got := resolveBracedEnvBody("PRESET_VAR:-fallback")
	if got != "from-env" {
		t.Errorf("got %q", got)
	}
}

// ----- shellEscape / shellEscapeValue / sliceHasComplexElement -----

func TestShellEscape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"hello", "'hello'"},
		{"hello world", "'hello world'"},
		{"it's mine", `'it'\''s mine'`},
		{"$VAR `cmd`", "'$VAR `cmd`'"},
		{"line\nbreak", "'line\nbreak'"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := shellEscape(c.in); got != c.want {
				t.Errorf("shellEscape(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestShellEscapeValue_Scalars(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"string", "hi", "'hi'"},
		{"int", 42, "'42'"},
		{"bool", true, "'true'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shellEscapeValue(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestShellEscapeValue_StringSlice(t *testing.T) {
	got := shellEscapeValue([]string{"a", "b c", "d"})
	if got != "'a' 'b c' 'd'" {
		t.Errorf("got %q", got)
	}
	if shellEscapeValue([]string{}) != "" {
		t.Error("empty []string should produce empty")
	}
}

func TestShellEscapeValue_HomogeneousInterfaceSlice(t *testing.T) {
	got := shellEscapeValue([]any{"a", 1, true})
	// Scalars → space-separated, each individually escaped.
	if got != "'a' '1' 'true'" {
		t.Errorf("got %q", got)
	}
}

func TestShellEscapeValue_ComplexSliceJSONEncoded(t *testing.T) {
	got := shellEscapeValue([]any{map[string]any{"k": "v"}, "x"})
	// Single JSON-encoded shell-quoted token.
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Errorf("expected single shell-quoted token, got %q", got)
	}
	if !strings.Contains(got, `{"k":"v"}`) {
		t.Errorf("expected JSON-encoded map inside, got %q", got)
	}
}

func TestShellEscapeValue_Map(t *testing.T) {
	got := shellEscapeValue(map[string]any{"k": "v"})
	// Map → JSON-encoded, shell-escaped.
	if !strings.Contains(got, `{"k":"v"}`) {
		t.Errorf("expected JSON-encoded map, got %q", got)
	}
}

func TestSliceHasComplexElement(t *testing.T) {
	if sliceHasComplexElement([]any{"a", 1, true}) {
		t.Error("scalars only should be reported as not complex")
	}
	if !sliceHasComplexElement([]any{"a", map[string]any{"k": "v"}}) {
		t.Error("map element should mark slice as complex")
	}
	if !sliceHasComplexElement([]any{[]any{1, 2}}) {
		t.Error("nested slice should mark slice as complex")
	}
}

// ----- scriptInterpreter -----

func TestScriptInterpreter(t *testing.T) {
	cases := []struct {
		lang    string
		wantCmd string
		wantExt string
	}{
		{"", "sh", ".sh"},
		{"sh", "sh", ".sh"},
		{"bash", "bash", ".sh"},
		{"js", "node", ".js"},
		{"node", "node", ".js"},
		{"py", "python3", ".py"},
		{"python", "python3", ".py"},
		{"python3", "python3", ".py"},
		{"unknown", "", ""},
		{"PowerShell", "", ""},
	}
	for _, c := range cases {
		t.Run(c.lang, func(t *testing.T) {
			cmd, ext := scriptInterpreter(c.lang)
			if cmd != c.wantCmd || ext != c.wantExt {
				t.Errorf("scriptInterpreter(%q) = (%q, %q), want (%q, %q)",
					c.lang, cmd, ext, c.wantCmd, c.wantExt)
			}
		})
	}
}

// ----- combineStreamsForLog -----

func TestCombineStreamsForLog(t *testing.T) {
	cases := []struct {
		name           string
		stdout, stderr string
		want           string
	}{
		{"both empty", "", "", ""},
		{"stdout only", "hello", "", "hello"},
		{"stderr only", "", "warn", "warn"},
		{"both present", "out", "err", "out\n--- stderr ---\nerr"},
		{"trailing newlines trimmed", "out\n", "err\n", "out\n--- stderr ---\nerr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := combineStreamsForLog(c.stdout, c.stderr); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ----- scriptRecipe: copy-based sandbox write-through -----

// fakeCopySandboxRun is a sandbox.Run whose workspace is a COPY of the
// host workspace (it implements WorkspaceFileRefresher, like the
// kubernetes driver). It records refreshed files and Exec calls.
type fakeCopySandboxRun struct {
	refreshed map[string][]byte
	execCalls [][]string
}

func (f *fakeCopySandboxRun) Driver() string { return "fake-copy" }
func (f *fakeCopySandboxRun) Command(ctx context.Context, cmd []string, _ sandbox.ExecOpts) *osexec.Cmd {
	return osexec.CommandContext(ctx, cmd[0], cmd[1:]...)
}
func (f *fakeCopySandboxRun) Exec(_ context.Context, cmd []string, _ sandbox.ExecOpts) (sandbox.ExecResult, error) {
	f.execCalls = append(f.execCalls, cmd)
	return sandbox.ExecResult{}, nil
}
func (f *fakeCopySandboxRun) Cleanup(context.Context) error { return nil }
func (f *fakeCopySandboxRun) RefreshWorkspaceFile(_ context.Context, relPath string, value []byte) error {
	if f.refreshed == nil {
		f.refreshed = map[string][]byte{}
	}
	f.refreshed[relPath] = value
	return nil
}

var _ sandbox.WorkspaceFileRefresher = (*fakeCopySandboxRun)(nil)

// A script tool node running against a copy-based sandbox must push the
// materialised script into the sandbox workspace (the host-side temp
// file is invisible there — the workspace was copied at Prepare time)
// and remove the in-sandbox copy on cleanup so a later `git add -A`
// cannot sweep it up.
func TestScriptRecipe_CopySandboxWriteThrough(t *testing.T) {
	wd := t.TempDir()
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(wd))
	fake := &fakeCopySandboxRun{}
	e.SetSandbox(fake)

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "script_node"},
		Language: "python3",
		Script:   "print('hi')",
	}
	resolve, buildCmd := e.scriptRecipe(context.Background(), node, map[string]any{})
	cmd, cleanup, err := buildCmd(resolve())
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	if cmd == nil {
		t.Fatal("buildCmd returned nil cmd")
	}
	if len(fake.refreshed) != 1 {
		t.Fatalf("refreshed files = %d, want 1 (%v)", len(fake.refreshed), fake.refreshed)
	}
	var base string
	for p, body := range fake.refreshed {
		base = p
		if filepath.Dir(p) != "." {
			t.Errorf("refresh path %q is not a basename", p)
		}
		if string(body) != "print('hi')" {
			t.Errorf("refreshed body = %q, want script body", body)
		}
	}
	// Host temp file exists until cleanup.
	if _, serr := os.Stat(filepath.Join(wd, base)); serr != nil {
		t.Fatalf("host temp script missing before cleanup: %v", serr)
	}
	cleanup()
	if _, serr := os.Stat(filepath.Join(wd, base)); !os.IsNotExist(serr) {
		t.Errorf("host temp script still present after cleanup")
	}
	found := false
	for _, call := range fake.execCalls {
		if len(call) == 3 && call[0] == "rm" && call[1] == "-f" && call[2] == base {
			found = true
		}
	}
	if !found {
		t.Errorf("cleanup did not rm the in-sandbox script (exec calls: %v)", fake.execCalls)
	}
}
