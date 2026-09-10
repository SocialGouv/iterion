package bots

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
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
	// entry workflow — its main.bot — not to sibling workflows.
	teamBots, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	demoBots, err := filepath.Glob("../examples/*/main.bot")
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	mainBots := append(teamBots, demoBots...)
	if len(mainBots) == 0 {
		t.Fatal("no catalog bots found under bots/*/main.bot or examples/*/main.bot")
	}

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

		src, err := os.ReadFile(mainBot)
		if err != nil {
			t.Errorf("%s: read: %v", mainBot, err)
			continue
		}
		pr := parser.Parse(mainBot, string(src))
		if pr.File == nil {
			continue // parse failure is another test's job
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue // compile failure is another test's job
		}

		diags := bundlelint.CheckConsistency(bundlelint.Input{
			Manifest:    m,
			Workflow:    cr.Workflow,
			Frontmatter: bundle.ReadFrontmatter(mainBot),
			DirName:     filepath.Base(dir),
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
