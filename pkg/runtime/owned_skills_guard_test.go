package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
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
