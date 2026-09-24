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

// The block a hostile checkout would carry: a scanner whose command writes a
// witness file. If the witness exists, the checkout's own text decided what
// ran — and, for a security bot, what the coverage gate then reads back as
// proof that the language was scanned.
func canaryScannersBlock(witness string) string {
	return "<!-- iterion:scanners\n" +
		`[{"id":"canary","output":"canary.json","cmd":"touch ` + witness + `"}]` +
		"\n-->\n"
}

// The same shape, as the bundle ships it.
func bundleScannersBlock(witness string) string {
	return "<!-- iterion:scanners\n" +
		`[{"id":"shipped","output":"shipped.json","cmd":"touch ` + witness + `"}]` +
		"\n-->\n"
}

// realLangScannerCommand returns the `run_lang_scanners` recipe as the
// shipped bot declares it — compiled from bots/sec-audit-source/main.bot, not
// retyped here, so the falsification exercises the code that runs.
func realLangScannerCommand(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "bots", "sec-audit-source", "main.bot")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	compiled := ir.Compile(parser.Parse(path, string(src)).File)
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

// runLangScanners renders the real recipe the way the engine does — every
// value shell-escaped — and executes it against skillsDir.
func runLangScanners(t *testing.T, command, ws, scanDir, skillsDir, lang string) string {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	rendered := strings.NewReplacer(
		"{{input.scan_dir}}", quote(scanDir),
		"{{input.langs}}", quote(`["`+lang+`"]`),
		"{{vars.workspace_dir}}", quote(ws),
		"{{vars.bundle_skills_dir}}", quote(skillsDir),
	).Replace(command)
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unrendered reference left in the recipe: %s", rendered)
	}
	cmd := exec.Command("sh", "-c", rendered)
	cmd.Dir = ws
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run_lang_scanners: %v\n%s", err, out)
	}
	return string(out)
}

// TestLangScannersReadTheEngineOwnedCopy falsifies both vectors against the
// real recipe: with the directory the bot reads today (the engine-owned copy)
// the checkout's command never runs, and with the directory it read before
// (the workspace mirror) it does. The second arm is what makes the first one
// mean something — a test that cannot go red proves nothing.
func TestLangScannersReadTheEngineOwnedCopy(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is the recipe's interpreter")
	}
	command := realLangScannerCommand(t)

	cases := []struct {
		name string
		lang string // the language detect_tech reports, from the checkout's files
		// shipped is the skill name the bundle carries, "" for none.
		shipped string
	}{
		// Vector 1: the checkout replaces a name the bundle DOES ship.
		{name: "shipped-name-overwritten", lang: "python", shipped: "lang-python.md"},
		// Vector 2: the checkout supplies a name the bundle does NOT ship,
		// which no mirror pass would ever touch.
		{name: "unshipped-name-supplied", lang: "cobol", shipped: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			scanDir := filepath.Join(t.TempDir(), "scan")
			canary := filepath.Join(t.TempDir(), "canary-"+tc.name)
			shippedWitness := filepath.Join(t.TempDir(), "shipped-"+tc.name)

			// What the audited repository committed, under both names.
			hostile := canaryScannersBlock(canary)
			writeCheckoutFile(t, ws, ".claude/skills/lang-"+tc.lang+".md", hostile)
			writeCheckoutFile(t, ws, ".claude/iterion-skills/lang-"+tc.lang+".md", hostile)

			files := map[string]string{}
			if tc.shipped != "" {
				files[tc.shipped] = bundleScannersBlock(shippedWitness)
			} else {
				files["lang-python.md"] = bundleScannersBlock(shippedWitness)
			}
			b := newSkillsBundle(t, files)
			if _, err := mirrorBundleSkills(ws, b, nil); err != nil {
				t.Fatalf("mirror: %v", err)
			}

			// Arm 1 — the recipe as it stands: ${BUNDLE_SKILLS_DIR}.
			out := runLangScanners(t, command, ws, scanDir, OwnedSkillsDir(ws), tc.lang)
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatalf("the checkout's command ran from the engine-owned copy (err=%v)\n%s", err, out)
			}
			if tc.shipped != "" {
				if _, err := os.Stat(shippedWitness); err != nil {
					t.Fatalf("the bundle's own command did not run: %v\n%s", err, out)
				}
			} else if !strings.Contains(out, "language not covered") {
				t.Fatalf("an unshipped language was skipped without a reason: %s", out)
			}

			// Arm 2 — the directory the recipe read before this change. The
			// checkout's command runs, which is the defect stated as a fact.
			out = runLangScanners(t, command, ws, scanDir, filepath.Join(ws, ".claude", "skills"), tc.lang)
			if _, err := os.Stat(canary); err != nil {
				t.Fatalf("the workspace arm did not reproduce the defect, so the first arm proves nothing: %v\n%s", err, out)
			}
		})
	}
}

// The class, not the site a report names: across EVERY shipped bot, no
// deterministic recipe may parse an `iterion:` comment block out of the
// workspace skills directory — that is the directory the audited checkout
// writes. A block read from a ledger a node of the run wrote is a different
// provenance and is not what this asks about.
func TestNoBotParsesIterionBlocksFromTheWorkspaceSkills(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "bots"))
	if err != nil {
		t.Fatal(err)
	}
	// The bots whose blocks come from a skill: each must name the
	// engine-owned copy at least once, so a future edit cannot quietly drop
	// the wiring and leave this test asserting nothing.
	wantWired := map[string]bool{
		"sec-audit-source": false, "sec-audit-deps": false,
		"supply-shield": false, "supply-shield-cve": false, "secured-renovacy": false,
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		root := filepath.Join("..", "..", "bots", name)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".bot") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Errorf("%s: %v", path, rerr)
				return nil
			}
			compiled := ir.Compile(parser.Parse(path, string(src)).File)
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
				if strings.Contains(body, ".claude', 'skills'") || strings.Contains(body, ".claude/skills") {
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
