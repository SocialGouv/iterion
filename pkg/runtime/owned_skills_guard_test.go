package runtime

import (
	"context"
	"fmt"
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
	// A relative workDir is RESOLVED, not refused: the caller means it against
	// the process's own directory, and the contract is that the ANSWER is
	// absolute, not that the caller spelled it that way.
	for _, workDir := range []string{".", "relative/dir"} {
		got := OwnedSkillsDir(workDir)
		if !filepath.IsAbs(got) {
			t.Errorf("OwnedSkillsDir(%q) = %q, want an absolute path", workDir, got)
		}
		if !strings.HasSuffix(got, filepath.Join(".claude", ownedSkillsDirName)) {
			t.Errorf("OwnedSkillsDir(%q) = %q, want it to end at the owned dir", workDir, got)
		}
	}
	if got := OwnedSkillsDir(""); got != "" {
		t.Errorf("OwnedSkillsDir(\"\") = %q, want \"\": there is no workspace to hold one", got)
	}
	// The container side has no process directory to resolve against, so a
	// relative pathname there is nothing.
	for _, ws := range []string{"", "workspace", "./workspace"} {
		if got := ownedSkillsContainerDir(ws); got != "" {
			t.Errorf("ownedSkillsContainerDir(%q) = %q, want \"\"", ws, got)
		}
	}
	abs := t.TempDir()
	if got := OwnedSkillsDir(abs); got != filepath.Join(abs, ".claude", ownedSkillsDirName) {
		t.Fatalf("OwnedSkillsDir(%q) = %q", abs, got)
	}
}

// An empty workspace keeps the mirror's historical no-op — there is no tree to
// hold the directory, and the reader's own guard is what refuses the empty
// expansion, by name. A relative one is resolved, not refused: a dispatcher
// spec, a dry run or a subbot request handing the engine one must keep working.
func TestMirrorAcceptsAnEmptyOrRelativeWorkspace(t *testing.T) {
	if _, err := mirrorBundleSkills("", nil, nil); err != nil {
		t.Fatalf("an empty workspace is a no-op, not a refusal: %v", err)
	}
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	b := newSkillsBundle(t, map[string]string{"lang-python.md": shippedBlock})
	if _, err := mirrorBundleSkills("sub/ws", b, nil); err != nil {
		t.Fatalf("a relative workspace was refused: %v", err)
	}
	// Resolved against the process directory, and the bundle landed there.
	if _, err := os.Stat(filepath.Join(dir, "sub", "ws", ".claude", ownedSkillsDirName, "lang-python.md")); err != nil {
		t.Fatalf("the owned copy did not land under the resolved workspace: %v", err)
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

// failingPodRun refuses to write files under the engine-owned copy. The prune
// already emptied it in the container, so a refill that gave up quietly would
// leave the child with no data blocks at all — and a reader that finds no
// entry for a name reports it NOT COVERED, which is a security verdict
// downgraded in silence. Adoption must fail closed at both halves.
type failingPodRun struct {
	*podFakeRun
	refuse string
}

func (f *failingPodRun) RefreshWorkspaceFile(ctx context.Context, rel string, value []byte) error {
	if strings.Contains(rel, f.refuse) {
		return fmt.Errorf("simulated write-through failure for %s", rel)
	}
	return f.podFakeRun.RefreshWorkspaceFile(ctx, rel, value)
}

func TestAdoptionFailsWhenTheOwnedCopyCannotLand(t *testing.T) {
	st := tmpStore(t)
	ctx := context.Background()
	pod := t.TempDir()
	workDir := t.TempDir()
	if _, err := mirrorBundleSkills(workDir, newSkillsBundle(t, map[string]string{"lang-python.md": shippedBlock}), nil); err != nil {
		t.Fatal(err)
	}
	fake := &failingPodRun{podFakeRun: &podFakeRun{pod: pod}, refuse: ownedSkillsDirName}
	ex := &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}
	e := New(&ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}}, st, ex,
		WithWorkDir(workDir),
		WithParentRunID("run-parent"),
		WithSharedSandbox(&SharedSandbox{Run: fake, WorkspaceFolder: pod}),
	)
	if _, err := st.CreateRun(ctx, "run-child", "child", nil); err != nil {
		t.Fatal(err)
	}
	_, err := e.startSandbox(ctx, "run-child", workDir, "", nil)
	if err == nil {
		t.Fatal("adoption proceeded with an owned copy that never landed")
	}
	if !strings.Contains(err.Error(), "not covered") {
		t.Fatalf("refusal does not name the consequence: %v", err)
	}
}

// A file OUTSIDE the owned copy keeps the documented behaviour: a skill the
// agent cannot read is a degraded run, not a dead one. The two classes must
// not be collapsed into one rule by a later edit.
func TestAdoptionToleratesAFailedAgentSkillWriteThrough(t *testing.T) {
	st := tmpStore(t)
	ctx := context.Background()
	pod := t.TempDir()
	workDir := t.TempDir()
	if _, err := mirrorBundleSkills(workDir, newSkillsBundle(t, map[string]string{"lang-python.md": shippedBlock}), nil); err != nil {
		t.Fatal(err)
	}
	fake := &failingPodRun{podFakeRun: &podFakeRun{pod: pod}, refuse: "lang-python/SKILL.md"}
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
		t.Fatalf("an agent-facing skill that did not land must degrade, not kill: %v", err)
	}
	cleanup()
}
