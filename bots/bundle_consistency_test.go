package bots

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/skilllib"
	"github.com/SocialGouv/iterion/pkg/subbotcontracts"
)

// consistencyAllowlist records (bot dir, diagnostic code) pairs that are
// known, accepted manifest↔workflow warnings pending a fix. Keep it
// SHRINKING — every entry is debt. ERRORS (e.g. C230) must NEVER be
// allowlisted: they indicate a real break (a split per-bot memory tree).
// Currently empty: the catalog is clean.
var consistencyAllowlist = map[string]map[string]bool{}

func consistencyAllowed(bot, code string) bool {
	return consistencyAllowlist[bot][code]
}

// TestCatalogBotsBundleConsistencyClean is the CI gate for the bundlelint
// (manifest↔workflow) checks: every shipped bundle must cross-check clean.
// A new dispatch_vars/context_vars/args_var/forge.secret/capability typo —
// or a per-bot-memory name mismatch — fails here, the same way the
// repo-agnostic and stack-agnostic guards next to it catch overfit.
func TestCatalogBotsBundleConsistencyClean(t *testing.T) {
	// The productised team lives under bots/ (this package's dir); the
	// remaining single-file catalog bots under examples/ are loose .bot
	// files with no manifest, so they have nothing to cross-check.
	// The bundle manifest (dispatch_vars, invocations) binds to the bundle's
	// entry workflow — its main.bot — not to sibling workflows; the syntax
	// floor (Input.Syntax) alone reads every root-level .bot of the bundle.
	// So the guard reads the ENTRY roots (teamEntryFiles/exampleEntryFiles),
	// each floored under its own discovery: a rename that stops a main.bot
	// spelling matching reddens there, naming the root, instead of leaving
	// this cross-check green over nothing.
	mainBots := append(teamEntryFiles(t), exampleEntryFiles(t)...)

	checked := 0
	for _, mainBot := range mainBots {
		dir := filepath.Dir(mainBot)
		manifestPath := filepath.Join(dir, "manifest.yaml")
		if _, statErr := os.Stat(manifestPath); statErr != nil {
			continue // loose .bot (no bundle manifest) — nothing to cross-check
		}

		m, err := bundle.LoadManifest(manifestPath)
		if err != nil {
			t.Errorf("%s: load manifest: %v", manifestPath, err)
			continue
		}
		if m == nil {
			continue
		}

		pr := parseBotUnit(mainBot)
		if pr.File == nil {
			continue // parse failure is another test's job
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue // compile failure is another test's job
		}

		fm, err := bundle.ReadFrontmatter(mainBot)
		if err != nil {
			t.Errorf("%s: frontmatter: %v", mainBot, err)
			continue
		}
		diags := bundlelint.CheckConsistency(bundlelint.Input{
			Manifest:    m,
			Workflow:    cr.Workflow,
			Frontmatter: fm,
			DirName:     filepath.Base(dir),
			// A shipped bundle using a syntax with a floor declares it
			// (C252 in CI): a catalogue bot a runner cannot parse is a
			// broken bot, not a demo.
			Syntax: bundle.MaxSyntaxRequirementsDir(dir),
			// The children a `subbot source:` names within the collection,
			// compiled for their contract (C255): the gate holds a shipped
			// parent to what its child declares, as `iterion validate` does.
			SubbotContracts: subbotcontracts.Read(dir, mainBot, cr.Workflow),
			// The bundle's own skill pages, read as `iterion validate` reads
			// them (cli.scanBundleSkills): without them the routability
			// checks (C231–C234) and the durable-memory reference check
			// (C274) were SKIPPED here, so this gate spoke for six of the
			// lint's checks while four of them judged nothing.
			Skills: bundleSkillDocs(t, dir),
		})
		checked++
		for _, d := range diags {
			if d.Severity != bundlelint.SeverityError && consistencyAllowed(filepath.Base(dir), string(d.Code)) {
				continue
			}
			t.Errorf("%s: %s\n  (fix the manifest/workflow; if this is accepted debt, add a consistencyAllowlist entry — but never for an error)", dir, d.Error())
		}
	}
	if checked == 0 {
		t.Fatal("no bundles cross-checked — discovery glob likely broke")
	}
}

// bundleSkillDocs reads one bundle's skill pages the way `iterion validate`
// does (pkg/cli.scanBundleSkills): skilllib.ListDirPages — THE reading of a
// skills/ directory, both canonical forms — its frontmatter parser, plus the
// page's text for the checks that ask which tools a page NAMES. A bundle with
// no skills/ dir yields none, and the skill checks then no-op.
//
// The FORM comes from the shared enumerator rather than a loop of its own:
// the copy this replaced skipped directory entries, which left the lint's
// whole skill family (C231–C234, C274) blind to `skills/<name>/SKILL.md` on
// both sides at once (iterion#2832).
func bundleSkillDocs(t *testing.T, dir string) []bundlelint.SkillDoc {
	t.Helper()
	skillsDir := filepath.Join(dir, bundle.DirSkills)
	pages, err := skilllib.ListDirPages(skillsDir)
	if err != nil {
		t.Fatalf("read %s: %v", skillsDir, err)
	}
	var docs []bundlelint.SkillDoc
	for _, p := range pages {
		body, err := os.ReadFile(p.Path)
		if err != nil {
			t.Fatalf("read %s: %v", p.Path, err)
		}
		name, desc := skilllib.ScanFrontmatter(strings.NewReader(string(body)))
		docs = append(docs, bundlelint.SkillDoc{
			Path:        bundle.DirSkills + "/" + p.Rel,
			Name:        name,
			Description: desc,
			Body:        string(body),
		})
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	return docs
}
