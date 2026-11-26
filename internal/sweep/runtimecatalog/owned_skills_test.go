package runtimecatalog

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
)

// The block a hostile checkout would carry: a scanner whose command writes a
// witness file. If the witness exists, the checkout's own text decided what
// ran — and, for a security bot, what the coverage gate then reads back as
// proof that the language was scanned.
func canaryScannersBlock(witness string) string {
	return "<!-- iterion:scanners\n" +
		`[{"id":"canary","output":"canary.json","cmd":"touch ` + witness + `"}]` +
		"\n-->\n"
}

// realLangScannerCommand returns the `run_lang_scanners` recipe as the
// shipped bot declares it — compiled from bots/sec-audit-source/main.bot, not
// retyped here, so the falsification exercises the code that runs.
func realLangScannerCommand(t *testing.T) string {
	t.Helper()
	path := filepath.Join(sweepwalk.ModuleRoot(t), "bots", "sec-audit-source", "main.bot")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	compiled := ir.Compile(parsertest.Parse(path, string(src)).File)
	if compiled.Workflow == nil {
		t.Fatalf("compile: %+v", compiled.Diagnostics)
	}
	for _, n := range compiled.Workflow.Nodes {
		tool, ok := n.(*ir.ToolNode)
		if ok && tool.ID == "run_lang_scanners" {
			return tool.Command
		}
	}
	t.Fatal("run_lang_scanners not found in bots/sec-audit-source/main.bot")
	return ""
}

// workspaceSkillsRead is a recipe naming the workspace skills directory, in
// the spellings a script builds it with: one path string, or its two
// segments joined by the language (`".claude", "skills"` in either quote
// style). Matching one quote style only let a bot that spelled it the other
// way read its blocks from the checkout with this guard green.
var workspaceSkillsRead = regexp.MustCompile(`\.claude/skills|\.claude["']\s*,\s*["']skills`)

// The class, not the site a report names: across EVERY shipped bot, no
// deterministic recipe may parse an `iterion:` comment block out of the
// workspace skills directory — that is the directory the audited checkout
// writes. A block read from a ledger a node of the run wrote is a different
// provenance and is not what this asks about.
func TestNoBotParsesIterionBlocksFromTheWorkspaceSkills(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(sweepwalk.ModuleRoot(t), "bots"))
	if err != nil {
		t.Fatal(err)
	}
	// The bots whose blocks come from a skill: each must name the
	// engine-owned copy at least once, so a future edit cannot quietly drop
	// the wiring and leave this test asserting nothing.
	wantWired := map[string]bool{
		"sec-audit-source": false, "sec-audit-deps": false,
		"supply-shield": false, "supply-shield-cve": false, "secured-renovacy": false,
		"assessment": false,
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		root := filepath.Join(sweepwalk.ModuleRoot(t), "bots", name)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".bot") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Errorf("%s: %v", path, rerr)
				return nil
			}
			compiled := ir.Compile(parsertest.Parse(path, string(src)).File)
			if compiled.Workflow == nil {
				return nil // a fragment that does not compile standalone
			}
			for _, n := range compiled.Workflow.Nodes {
				tool, ok := n.(*ir.ToolNode)
				if !ok {
					continue
				}
				body := tool.Command + "\n" + tool.Script
				if strings.Contains(body, "{{vars.bundle_skills_dir}}") {
					if _, tracked := wantWired[name]; tracked {
						wantWired[name] = true
					}
				}
				// An `iterion:` block parse spells the comment delimiter; a
				// prose mention of the marker does not.
				if !strings.Contains(body, "iterion:") || !strings.Contains(body, "<!--") {
					continue
				}
				if workspaceSkillsRead.MatchString(body) {
					t.Errorf("%s node %q parses an iterion: block from the workspace skills directory (%s)", name, tool.ID, path)
				}
			}
			return nil
		})
	}
	for name, wired := range wantWired {
		if !wired {
			t.Errorf("%s no longer reads any iterion: block from ${BUNDLE_SKILLS_DIR}", name)
		}
	}
}

// whichThenWorkspaceJoin matches the defect shape of both #1799 sites: a
// binary resolved by shutil.which OR a path joined onto the workspace root —
// the workspace being a checkout of an audited/untrusted repository, so the
// joined file is one the repository supplies. Matches across quote styles
// and across the line break the long spellings put before os.path.join.
var whichThenWorkspaceJoin = regexp.MustCompile(
	`shutil\.which\(\s*['"][^'"]+['"]\s*\)\s*or\s*os\.path\.join\(`)

// The class, not the sites a report names: across EVERY shipped bot, no
// deterministic recipe may resolve an executable as "the one on PATH, or a
// file in the workspace". PATH (engine-shimmed on the host, baked in the
// container) and the engine-written ITERION_ENGINE_BIN are the sources; a
// workspace fallback executes the tree under audit with the run's
// credentials (#1799).
func TestNoBotResolvesABinaryFromTheWorkspace(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(sweepwalk.ModuleRoot(t), "bots"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		root := filepath.Join(sweepwalk.ModuleRoot(t), "bots", name)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".bot") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Errorf("%s: %v", path, rerr)
				return nil
			}
			compiled := ir.Compile(parsertest.Parse(path, string(src)).File)
			if compiled.Workflow == nil {
				return nil // a fragment that does not compile standalone
			}
			for _, n := range compiled.Workflow.Nodes {
				tool, ok := n.(*ir.ToolNode)
				if !ok {
					continue
				}
				body := tool.Command + "\n" + tool.Script
				if whichThenWorkspaceJoin.MatchString(body) {
					t.Errorf("%s node %q resolves a binary as PATH-or-workspace-join (%s) — drop the workspace fallback; fail loudly instead (#1799)", name, tool.ID, path)
				}
			}
			return nil
		})
	}
}

// Every bot declaring the var must still COMPILE with it — the measurement
// that decided the pattern's shape. The default `${BUNDLE_SKILLS_DIR}` is a
// live reference, so its pattern is checked at launch (C181 names that) and
// what a compile can refuse here is the declaration itself: a pattern that is
// not valid RE2 (C162) or a literal default off its own pattern (C161).
func TestBundleSkillsVarPatternAdmitsItsOwnDefault(t *testing.T) {
	for _, bot := range []string{"sec-audit-source", "sec-audit-deps", "supply-shield", "supply-shield-cve", "secured-renovacy"} {
		path := filepath.Join(sweepwalk.ModuleRoot(t), "bots", bot, "main.bot")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		compiled := ir.Compile(parsertest.Parse(path, string(src)).File)
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
