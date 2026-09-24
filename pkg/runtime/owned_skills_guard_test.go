package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// The value can be neither empty nor relative
// ---------------------------------------------------------------------------

// A reader joins a skill NAME onto the directory, so an empty or relative
// value resolves against the node's working directory — the checkout. The
// engine never produces such a value and refuses the run instead.
func TestOwnedSkillsDirIsAbsoluteOrNothing(t *testing.T) {
	for _, workDir := range []string{"", ".", "relative/dir"} {
		if got := OwnedSkillsDir(workDir); got != "" {
			t.Errorf("OwnedSkillsDir(%q) = %q, want \"\": a non-absolute answer resolves against the reader's cwd", workDir, got)
		}
	}
	for _, ws := range []string{"", "workspace", "./workspace"} {
		if got := ownedSkillsContainerDir(ws); got != "" {
			t.Errorf("ownedSkillsContainerDir(%q) = %q, want \"\"", ws, got)
		}
	}
	abs := t.TempDir()
	if got := OwnedSkillsDir(abs); !filepath.IsAbs(got) {
		t.Fatalf("OwnedSkillsDir(%q) = %q, want an absolute path", abs, got)
	}
}

// A run whose workspace cannot be named absolutely is refused at the mirror,
// where the failure is fatal — not allowed to proceed with an expansion that
// would send every reader to its own working directory.
func TestMirrorRefusesARunWithNoAbsoluteWorkspace(t *testing.T) {
	for _, workDir := range []string{"", "relative/dir"} {
		_, err := mirrorBundleSkills(workDir, nil, nil)
		if err == nil {
			t.Fatalf("workDir %q: mirror accepted a workspace it cannot name", workDir)
		}
		if !strings.Contains(err.Error(), "BUNDLE_SKILLS_DIR") || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("workDir %q: refusal does not name what is wrong: %v", workDir, err)
		}
	}
}

// The launch gate refuses an override that would empty or relativise the
// value. It reads the EXPANDED value, so the literal a studio form re-sends
// unmodified passes while "" and "some/dir" do not.
//
// The pattern's second branch is not decoration: the compile-time default
// check (C161) compares the LITERAL default text, so a bare "^/" would refuse
// `${BUNDLE_SKILLS_DIR}` itself. TestBundleSkillsVarPatternAdmitsItsOwnDefault
// holds that end; this one holds the launch end.
func TestBundleSkillsVarRefusesANonAbsoluteOverride(t *testing.T) {
	for _, bot := range []string{"sec-audit-source", "sec-audit-deps", "supply-shield", "supply-shield-cve", "secured-renovacy"} {
		path := filepath.Join("..", "..", "bots", bot, "main.bot")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		compiled := ir.Compile(parser.Parse(path, string(src)).File)
		if compiled.Workflow == nil {
			t.Fatalf("%s: compile: %+v", bot, compiled.Diagnostics)
		}
		vars := compiled.Workflow.Vars
		if vars["bundle_skills_dir"] == nil || vars["bundle_skills_dir"].Matching == "" {
			t.Fatalf("%s: bundle_skills_dir carries no pattern, so a launch override is unchecked", bot)
		}
		ws := t.TempDir()
		e := &Engine{workDir: ws}
		for _, bad := range []string{"", "some/dir", "..", "./x"} {
			if err := ValidateVarConstraints(vars, map[string]any{"bundle_skills_dir": bad}, e.varExpandFn()); err == nil {
				t.Errorf("%s: launch accepted bundle_skills_dir=%q", bot, bad)
			}
		}
		for _, good := range []string{"${BUNDLE_SKILLS_DIR}", filepath.Join(ws, ".claude", "iterion-skills")} {
			if err := ValidateVarConstraints(vars, map[string]any{"bundle_skills_dir": good}, e.varExpandFn()); err != nil {
				t.Errorf("%s: launch refused bundle_skills_dir=%q: %v", bot, good, err)
			}
		}
	}
}

// Every bot declaring the var must still COMPILE with it — the measurement
// that decided the pattern's shape. C161 reads the literal default, so a
// pattern that admits only "^/" fails here even though the expansion is
// always absolute.
func TestBundleSkillsVarPatternAdmitsItsOwnDefault(t *testing.T) {
	for _, bot := range []string{"sec-audit-source", "sec-audit-deps", "supply-shield", "supply-shield-cve", "secured-renovacy"} {
		path := filepath.Join("..", "..", "bots", bot, "main.bot")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		compiled := ir.Compile(parser.Parse(path, string(src)).File)
		for _, d := range compiled.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Fatalf("%s: %s: %s", bot, d.Code, d.Message)
			}
		}
	}
}

// The last layer, exercised on the recipe the bot ships: handed a value that
// is not an absolute path, the node refuses by name instead of joining the
// skill name onto nothing and reading the checkout's own file.
func TestLangScannersRefuseANonAbsoluteSkillsDir(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is the recipe's interpreter")
	}
	command := realLangScannerCommand(t)
	ws := t.TempDir()
	canary := filepath.Join(t.TempDir(), "canary-relative")
	// The file a checkout would place at its own root, which a relative
	// join reaches from the node's working directory.
	writeCheckoutFile(t, ws, "lang-python.md", canaryScannersBlock(canary))

	for _, bad := range []string{"", "."} {
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
		rendered := strings.NewReplacer(
			"{{input.scan_dir}}", quote(filepath.Join(t.TempDir(), "scan")),
			"{{input.langs}}", quote(`["python"]`),
			"{{vars.workspace_dir}}", quote(ws),
			"{{vars.bundle_skills_dir}}", quote(bad),
		).Replace(command)
		cmd := exec.Command("sh", "-c", rendered)
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("bundle_skills_dir=%q: the node accepted it\n%s", bad, out)
		}
		if !strings.Contains(string(out), "not an absolute path") {
			t.Fatalf("bundle_skills_dir=%q: refusal is not named: %s", bad, out)
		}
		if _, serr := os.Stat(canary); !os.IsNotExist(serr) {
			t.Fatalf("bundle_skills_dir=%q: the checkout's command ran (err=%v)\n%s", bad, serr, out)
		}
	}
}

// ---------------------------------------------------------------------------
// A child never inherits the parent's owned copy
// ---------------------------------------------------------------------------

// podFakeRun stands in for a copy-based parent sandbox whose workspace is a
// real directory: `rm` and the write-through seam both act on it, so a prune
// that does not happen is visible as a file that survives.
type podFakeRun struct{ pod string }

func (p *podFakeRun) Driver() string { return "fake-pod" }
func (p *podFakeRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = p.pod
	return c
}
func (p *podFakeRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	out, err := p.Command(ctx, cmd, opts).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return sandbox.ExecResult{ExitCode: code, Stdout: out, Stderr: out}, nil
}
func (p *podFakeRun) Cleanup(context.Context) error { return nil }
func (p *podFakeRun) RefreshWorkspaceFile(_ context.Context, rel string, value []byte) error {
	target := filepath.Join(p.pod, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, value, 0o644)
}

var _ sandbox.WorkspaceFileRefresher = (*podFakeRun)(nil)

// A child adopting its parent's live copy-based sandbox must not read a skill
// name only the PARENT's bundle ships. The write-through seam adds files and
// removes none, so the directory is emptied before the child's copy lands.
func TestAdoptedChildDoesNotInheritTheParentsOwnedSkills(t *testing.T) {
	st := tmpStore(t)
	ctx := context.Background()

	// The parent's pod: its own bundle's copy is already there.
	pod := t.TempDir()
	podOwned := filepath.Join(pod, ".claude", ownedSkillsDirName)
	if err := os.MkdirAll(podOwned, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lang-parentonly.md", "lang-python.md"} {
		if err := os.WriteFile(filepath.Join(podOwned, name), []byte("# parent bundle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The child's host workdir: its OWN bundle ships only lang-python.md.
	workDir := t.TempDir()
	b := newSkillsBundle(t, map[string]string{"lang-python.md": shippedBlock})
	if _, err := mirrorBundleSkills(workDir, b, nil); err != nil {
		t.Fatal(err)
	}

	fake := &podFakeRun{pod: pod}
	ex := &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}
	e := New(&ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}}, st, ex,
		WithWorkDir(workDir),
		WithParentRunID("run-parent"),
		WithSharedSandbox(&SharedSandbox{Run: fake, WorkspaceFolder: pod}),
	)
	if _, err := st.CreateRun(ctx, "run-child", "child", nil); err != nil {
		t.Fatal(err)
	}
	cleanup, err := e.startSandbox(ctx, "run-child", workDir, "", nil)
	if err != nil {
		t.Fatalf("startSandbox: %v", err)
	}
	defer cleanup()

	if _, err := os.Stat(filepath.Join(podOwned, "lang-parentonly.md")); !os.IsNotExist(err) {
		t.Fatalf("the child inherited a skill name only the parent's bundle ships (err=%v)", err)
	}
	got, err := os.ReadFile(filepath.Join(podOwned, "lang-python.md"))
	if err != nil {
		t.Fatalf("the child's own copy did not land: %v", err)
	}
	if string(got) != shippedBlock {
		t.Fatalf("the parent's bytes answered for a name the child also ships: %q", got)
	}

	evs, err := st.LoadEvents(ctx, "run-child")
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, ev := range evs {
		if ev.Type == store.EventSandboxShared && ev.Data["owned_skills_pruned"] == true {
			said = true
		}
	}
	if !said {
		t.Fatal("the prune is not in the run record")
	}
}

// A shared sandbox that cannot name its workspace absolutely is refused
// rather than adopted: the parent's copy would stay, unlocatable and unread
// by anyone who could remove it.
func TestAdoptionRefusesAnUnlocatableSharedWorkspace(t *testing.T) {
	st := tmpStore(t)
	ctx := context.Background()
	workDir := t.TempDir()
	if _, err := mirrorBundleSkills(workDir, newSkillsBundle(t, map[string]string{"lang-python.md": shippedBlock}), nil); err != nil {
		t.Fatal(err)
	}
	fake := &podFakeRun{pod: t.TempDir()}
	ex := &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}
	e := New(&ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}}, st, ex,
		WithWorkDir(workDir),
		WithParentRunID("run-parent"),
		WithSharedSandbox(&SharedSandbox{Run: fake, WorkspaceFolder: "workspace"}),
	)
	if _, err := st.CreateRun(ctx, "run-child", "child", nil); err != nil {
		t.Fatal(err)
	}
	_, err := e.startSandbox(ctx, "run-child", workDir, "", nil)
	if err == nil {
		t.Fatal("adoption accepted a shared workspace it cannot name")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("refusal does not name what is wrong: %v", err)
	}
}
