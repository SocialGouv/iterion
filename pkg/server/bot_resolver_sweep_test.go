package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The bot-resolution authority is bot_resolver.go: every pkg/server site
// that turns a bot id into source, a manifest, or catalog metadata must go
// through it, or a platform override is silently ignored on that surface
// (grep-la-classe: the class is "sites that select a bot"). These two
// static sweeps keep the class closed as the package grows.

// resolverSweepAllowed lists the files that may call the raw botregistry
// discovery/lookup functions, with the reason.
var resolverSweepAllowed = map[string]string{
	"bot_resolver.go":         "the authority itself",
	"bots_tenant.go":          "materialized stored-bot discovery (feeds the authority)",
	"bots_routes.go":          "botListOptions/effectivePaths plumbing + baked-only write paths (bot editing writes the FS catalog)",
	"bots_create.go":          "scaffolds NEW bots into the workspace FS (no override can exist yet)",
	"bot_sources_routes.go":   "fork-from-catalog reads the BAKED bundle by definition",
	"server_dsl.go":           "studio Home display walker over FS load names (cosmetic; /bots gallery is the covered surface)",
	"catalog_regen.go":        "regenerates the FS catalog skill from the FS manifests by design",
	"config_shares_routes.go": "botManifest's baked-FS FALLBACK, reached after platformBotManifest",
	// The inversion the rest of this allowlist guards against IS this file's
	// job: it reads the baked bundle PAST the override, on purpose, to report
	// an override that shadows a newer bake. Routing it through the authority
	// would return the override and always compare a version with itself.
	"bot_override_staleness.go": "reads the baked catalog deliberately, to compare it AGAINST the override",
}

func TestBotResolutionSweep_NoRawRegistryReads(t *testing.T) {
	// Raw lookups that would bypass the platform tier. ResolveBotPath /
	// FindByName / List / ListWithSchema over the server's effective paths.
	raw := regexp.MustCompile(`botregistry\.(ResolveBotPath|FindByName|List|ListWithSchema)\(`)
	sweepServerFiles(t, func(name, body string) {
		if !raw.MatchString(body) {
			return
		}
		if _, ok := resolverSweepAllowed[name]; ok {
			return
		}
		t.Errorf("%s calls a raw botregistry lookup — route it through bot_resolver.go (resolveBotTiered / effectiveEntries*) or add an allowlist entry with its reason", name)
	})
}

// teamlessResolveAllowed lists the files that may resolve a bot with a
// LITERAL empty team, with the reason. Every launch surface knows the team it
// launches for; a hardcoded "" there drops the team tier for that surface
// alone — the #871 defect, which survived because four launchers were assumed
// to behave like the fifth.
var teamlessResolveAllowed = map[string]string{
	"resume_source.go": "the no-persisted-origin branch: a run whose BotSourceTenant is empty did NOT launch from a team row (the team branch above re-resolves that row by its own tenant)",
}

func TestBotResolutionSweep_NoLiteralTeamlessResolution(t *testing.T) {
	// Both halves of the class: the launch resolution AND the metadata reads
	// that describe the same launch (a lane that launches a team's fork and
	// then reads the origin's manifest disagrees with itself in silence).
	// The bare `resolveBot(` arm catches the trigger spine, which calls the
	// authority through an injected closure field rather than by name.
	// Only a LITERAL "" is decidable statically; a variable that happens to
	// be empty at run time is the caller's own contract to keep.
	teamless := regexp.MustCompile(
		`(?:resolveBot(?:Source|Tiered(?:Raw)?)?|resolveRunRetryPolicy|handoffConsumersFor|botVarDefault|botManifestFor|effectiveFindByNameForTeam)\([^,]+,\s*""`)
	sweepServerFiles(t, func(name, body string) {
		if !teamless.MatchString(body) {
			return
		}
		if _, ok := teamlessResolveAllowed[name]; ok {
			return
		}
		t.Errorf("%s resolves a bot with a hardcoded empty team — pass the team the launch is FOR (the card's, the subscription's, the schedule's, the webhook's) or add an allowlist entry saying why this surface has none", name)
	})
}

// The webhook role constants are config now (platformcfg bot_roles): a site
// that reads a constant directly re-hardcodes the role and silently ignores
// the operator's override.
var roleSweepAllowed = map[string]string{
	"webhooks_common.go":   "the const declarations (the defaults)",
	"platform_settings.go": "the resolver's default table",
}

func TestBotRoleSweep_ConstsOnlyInDefaults(t *testing.T) {
	consts := regexp.MustCompile(`defaultWebhookBotReviewPR|defaultWebhookBotReviConverse|branchImproveBotID|featureDevBotID`)
	constDecl := regexp.MustCompile(`(?m)^const (defaultWebhookBot|branchImproveBotID|featureDevBotID)`)
	sweepServerFiles(t, func(name, body string) {
		if !consts.MatchString(body) {
			return
		}
		if _, ok := roleSweepAllowed[name]; ok {
			if name == "webhooks_common.go" && !constDecl.MatchString(body) {
				t.Errorf("webhooks_common.go references a role const outside its declaration block")
			}
			return
		}
		t.Errorf("%s reads a role constant directly — use s.roleBots().<Role> so the platform override applies", name)
	})
}

// sweepServerFiles runs fn over every non-test .go file of this package,
// with comments stripped so prose mentions don't trip the sweep.
func sweepServerFiles(t *testing.T, fn func(name, body string)) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	lineComment := regexp.MustCompile(`//[^\n]*`)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		fn(name, lineComment.ReplaceAllString(string(b), ""))
	}
}
