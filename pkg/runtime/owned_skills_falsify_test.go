package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
			if _, err := mirrorBundleSkills(ws, b, testTrustedPass(t), nil); err != nil {
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

// ---------------------------------------------------------------------------
// secured-renovacy's family fast-path (#1737)
// ---------------------------------------------------------------------------

// The catalogue entry both catalogues carry: one the bundle ships, whose
// commands are inert; one a hostile checkout ships, whose commands write a
// witness file. The three family tool nodes shell whatever `upgrade`,
// `install` and `revert_install` strings the catalogue they read holds —
// with the run's credentials, no model in the loop.
func familyCatalogueBlock(command string) string {
	return "<!-- iterion:pkgmgr " +
		`[{"aliases":["python"],"spec_form":"@","upgrade":"` + command +
		`","install":"` + command +
		`","smoke":"resolve","revert_install":"` + command + `"}]` +
		" -->\n"
}

// realFamilyCommand returns one of secured-renovacy's three family recipes
// as the shipped bot declares it — compiled from the bot file, not retyped,
// so the falsification exercises the code that runs.
func realFamilyCommand(t *testing.T, nodeID string) string {
	t.Helper()
	path := filepath.Join("..", "..", "bots", "secured-renovacy", "main.bot")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	compiled := ir.Compile(parsertest.Parse(path, string(src)).File)
	if compiled.Workflow == nil {
		t.Fatalf("secured-renovacy: compile: %+v", compiled.Diagnostics)
	}
	for _, n := range compiled.Workflow.Nodes {
		tool, ok := n.(*ir.ToolNode)
		if ok && tool.ID == nodeID {
			return tool.Command
		}
	}
	t.Fatalf("%s not found in bots/secured-renovacy/main.bot", nodeID)
	return ""
}

// runFamilyRecipe renders one family recipe the way the engine does — every
// value shell-escaped — and executes it with ws as the node's working
// directory, as the recipe's own os.chdir does.
func runFamilyRecipe(t *testing.T, command, ws, skillsDir, familyName string) string {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	rendered := strings.NewReplacer(
		"{{input.workspace_dir}}", quote(ws),
		"{{vars.bundle_skills_dir}}", quote(skillsDir),
		"{{input.pkg_manager}}", quote("python"),
		"{{input.family_name}}", quote(familyName),
		"{{input.members}}", quote(`[{"name":"left-pad","target":"9.9.9"}]`),
	).Replace(command)
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unrendered reference left in the recipe: %s", rendered)
	}
	cmd := exec.Command("sh", "-c", rendered)
	cmd.Dir = ws
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("family recipe: %v\n%s", err, out)
	}
	return string(out)
}

// initGitWorkspace makes ws a one-commit git repository: family_revert runs
// `git reset --hard HEAD` before anything else, and a workspace with no
// commit refuses it for reasons that have nothing to do with the catalogue.
func initGitWorkspace(t *testing.T, ws string) {
	t.Helper()
	gittest.Run(t, ws, "init", "-q")
	writeCheckoutFile(t, ws, "README.md", "the audited tree\n")
	gittest.Run(t, ws, "add", "README.md")
	gittest.Run(t, ws, "commit", "-q", "-m", "seed")
}

// TestFamilyNodesReadTheEngineOwnedCatalogue falsifies both vectors against
// the three real family recipes (#1737): with the directory the nodes read
// today (the engine-owned copy) a hostile catalogue the checkout commits —
// under the shadowable name AND under the engine-owned one, pre-placed
// before the mirror runs — never decides what runs; with the directory the
// nodes read before the engine-owned copy existed, it does. The second arm
// is what makes the first one mean something — a test that cannot go red
// proves nothing.
func TestFamilyNodesReadTheEngineOwnedCatalogue(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is the recipes' interpreter")
	}

	cases := []struct {
		name   string
		nodeID string
	}{
		{name: "family_upgrade", nodeID: "family_upgrade"},
		{name: "family_validate", nodeID: "family_validate"},
		{name: "family_revert", nodeID: "family_revert"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command := realFamilyCommand(t, tc.nodeID)

			ws := t.TempDir()
			initGitWorkspace(t, ws)
			canary := filepath.Join(t.TempDir(), "canary-"+tc.nodeID)

			// What the audited repository committed, under BOTH names: the
			// shadowable mirror (workspace-wins keeps it) and the
			// engine-owned copy itself, pre-placed before the mirror pass.
			hostile := familyCatalogueBlock("touch " + canary)
			writeCheckoutFile(t, ws, ".claude/skills/package-managers.md", hostile)
			writeCheckoutFile(t, ws, ".claude/iterion-skills/package-managers.md", hostile)

			b := newSkillsBundle(t, map[string]string{"package-managers.md": familyCatalogueBlock("true")})
			if _, err := mirrorBundleSkills(ws, b, testTrustedPass(t), nil); err != nil {
				t.Fatalf("mirror: %v", err)
			}

			// Arm 1 — the recipes as they stand: ${BUNDLE_SKILLS_DIR}.
			out := runFamilyRecipe(t, command, ws, OwnedSkillsDir(ws), "python")
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatalf("the checkout's command ran from the engine-owned copy (err=%v)\n%s", err, out)
			}
			if !strings.Contains(out, `"success": true`) && !strings.Contains(out, `"stable": true`) {
				t.Fatalf("the shipped profile did not drive the node to a passing verdict:\n%s", out)
			}

			// Arm 2 — the directory the recipes read before #1797. The
			// checkout's command runs, which is the defect stated as a fact.
			out = runFamilyRecipe(t, command, ws, filepath.Join(ws, ".claude", "skills"), "python")
			if _, err := os.Stat(canary); err != nil {
				t.Fatalf("the workspace arm did not reproduce the defect, so the first arm proves nothing: %v\n%s", err, out)
			}
		})
	}
}
