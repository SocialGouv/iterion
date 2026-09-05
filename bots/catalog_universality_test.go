package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Catalog bots (the *.bot bundles `iterion bots list` discovers — the
// productised team under bots/ plus the remaining single-file bots under
// examples/) are general-purpose tools that must run on ANY target
// repository, in any language, with no knowledge of iterion's own
// layout baked in. See "Catalog bots are repo-agnostic" in CLAUDE.md.
//
// This test greps every catalog bot's typed var-default string
// literals for iterion *target-repo* facts. It guards the most
// common overfit — a bot author staring at the iterion tree
// hardcoding `cmd/iterion/*.go` / `.iterion/...` into a default that
// should be language/layout-agnostic. It deliberately inspects ONLY
// `name: type = "literal"` var defaults: comments may legitimately
// mention `cmd/iterion` as an example, and runtime references
// (mcp__iterion_board__*, `iterion report`, `.bot` DSL) are fine —
// the bot is written FOR iterion, it must not be scoped TO iterion.

// varDefaultRe matches a typed var default with a string literal:
//
//	doc_globs: string = "README.md,docs/**/*.md"
//
// Capture group 1 is the var name, group 2 the default value.
var varDefaultRe = regexp.MustCompile(
	`^\s+([A-Za-z_][A-Za-z0-9_]*):\s+(?:string|json)\s*=\s*"([^"]*)"`,
)

// violationPatterns are substrings that, when they appear in a var
// DEFAULT, scope the bot to iterion's own tree. Each one breaks a
// run against someone else's repo (the glob matches nothing, or the
// path scatters `.iterion/` into their working tree).
var violationPatterns = []string{
	"cmd/iterion",       // iterion's CLI package layout
	"pkg/dsl",           // iterion's DSL internals
	"pkg/**",            // iterion's Go pkg layout
	"cmd/**",            // iterion's Go cmd layout
	"examples/*/skills", // iterion's (legacy) bundle-doc layout
	"bots/*/skills",     // iterion's bundle-doc layout (post-relocation)
	".iterion/",         // writing iterion's store dir into the target repo
}

// allowlist records (bot dir, var name, pattern) triples that are
// known iterion-specific defaults still pending a universal rewrite.
// Keep this list EMPTY — every entry is debt, and any violation on
// any bot fails the test outright.
//
// Empty since the sec-audit relocation: per-run scratch (scanner
// output, deps package cache) moved to ${PROJECT_SCRATCH_DIR}/<bot>/,
// and durable operator-facing state (FP memory, file records,
// matchers, context, patches) moved to the target repo's neutral,
// gitignorable `.sec-audit/` dotdir. If you must add an entry back,
// it is a regression to justify in the entry's reason — and to shrink
// away again.
type allowEntry struct {
	bot, varName, pattern, reason string //nolint:unused // reason is the justification slot filled when an exemption is re-added (see doc above)
}

var allowlist = []allowEntry{}

func isAllowed(bot, varName, pattern string) bool {
	for _, a := range allowlist {
		if a.bot == bot && a.varName == varName && a.pattern == pattern {
			return true
		}
	}
	return false
}

func TestCatalogBotsAreRepoAgnostic(t *testing.T) {
	// The productised team lives under bots/ (this package's dir); the
	// remaining single-file catalog bots (e.g. clarify) stay under
	// examples/. Cover both so neither set can regress.
	teamBots, err := teamBotFiles()
	if err != nil {
		t.Fatalf("glob team bots: %v", err)
	}
	demoBots, err := filepath.Glob("../examples/*/main.bot")
	if err != nil {
		t.Fatalf("glob demo catalog bots: %v", err)
	}
	botPaths := append(teamBots, demoBots...)
	if len(botPaths) == 0 {
		t.Fatal("no catalog bots found under bots/*/main.bot or examples/*/main.bot")
	}

	for _, botPath := range botPaths {
		bot := filepath.Base(filepath.Dir(botPath))
		data, err := os.ReadFile(botPath)
		if err != nil {
			t.Errorf("%s: read: %v", botPath, err)
			continue
		}
		inVars := false
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			// Track the top-level `vars:` block: it opens on a line
			// that is exactly `vars:` and closes at the next
			// top-level (column-0) declaration.
			if trimmed == "vars:" && !strings.HasPrefix(line, " ") {
				inVars = true
				continue
			}
			if inVars && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				inVars = false
			}
			if !inVars {
				continue
			}
			m := varDefaultRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			varName, value := m[1], m[2]
			for _, pat := range violationPatterns {
				if !strings.Contains(value, pat) {
					continue
				}
				if isAllowed(bot, varName, pat) {
					continue
				}
				t.Errorf(
					"%s: var %q default %q hardcodes iterion target-repo pattern %q — catalog bots must be repo-agnostic (see CLAUDE.md \"Catalog bots are repo-agnostic\"). Default to a language/layout-agnostic value and make the iterion-specific scope a per-run --var override.",
					botPath, varName, value, pat,
				)
			}
		}
	}
}

// stackPattern is a high-signal indicator that a catalog bot hardcodes
// language/ecosystem logic in its DSL instead of delegating it to skills.
// These strings essentially never appear in prompt prose, so a whole-file
// line scan is reliable without block-aware parsing. See CLAUDE.md
// "Universal code bots — stack knowledge lives in skills".
type stackPattern struct {
	re   *regexp.Regexp
	what string
}

var stackPatterns = []stackPattern{
	{regexp.MustCompile(`case\s+"?\$\{?(PKG_MGR|pkg_manager)\}?"?\s+in`), "per-ecosystem shell case dispatch"},
	// Invocation-form (binary + a flag/path arg), so a prose comment that
	// merely NAMES a scanner ("gosec/semgrep can emit thousands…") is not a
	// violation — only an actual command is. The real invocations belong in
	// the bot's skills (which this test does not scan), not in the DSL.
	{regexp.MustCompile(`\bgosec\s+-`), "hardcoded Go SAST scanner invocation"},
	{regexp.MustCompile(`\bbandit\s+(-|--recursive)`), "hardcoded Python SAST scanner invocation"},
	{regexp.MustCompile(`\bgovulncheck\s+(-|\./)`), "hardcoded Go SCA scanner invocation"},
	{regexp.MustCompile(`\bpip-audit\s+-`), "hardcoded Python SCA scanner invocation"},
	{regexp.MustCompile(`\bnpm\s+audit\s+--`), "hardcoded npm SCA scanner invocation"},
	// Per-LANGUAGE semgrep packs (p/golang, p/python, …) enumerate a stack in
	// the DSL and belong in skills. p/default — Semgrep's universal
	// cross-language pack — is the always-on generic floor (the metrics-off
	// equivalent of --config=auto, which CLAUDE.md's "always-on generic floor"
	// explicitly allows: `--config=auto --metrics=off` is rejected by semgrep,
	// so the floor uses p/default instead). So match the language packs, not
	// p/default (nor non-language packs like p/ci, p/secrets).
	{regexp.MustCompile(`semgrep\s+--config=p/(golang|go|python|py|javascript|js|typescript|ts|java|ruby|php|rust|csharp|kotlin|scala|swift|elixir|bash)\b`), "hardcoded per-language semgrep ruleset"},
	{regexp.MustCompile(`^\s*has_(js|go|python|npm|pypi|gomod|rust|ruby|php|java)\s*:\s*bool`), "closed-enum tech boolean in schema (emit an open langs/ecosystems list)"},
	{regexp.MustCompile(`^(agent|tool|judge)\s+run_(js|go|py|python)_(scanners|heuristics)\s*:`), "per-language scanner/heuristic node (use one adaptive agent step)"},
}

// stackAgnosticExemptions lists catalog bots whose stack-specific DSL is known
// debt, pending migration to the skill-guided pattern. SHRINK this: every
// entry is a bot that still hardcodes a language/ecosystem. When a bot is
// migrated, remove it here — the test then guards it against regression. A bot
// left in the list that no longer matches any pattern fails the test, forcing
// the entry to be removed.
// Empty: every catalog bot now dispatches stack-specific work through skill
// data + deterministic gates, with zero language/ecosystem enumeration in the
// DSL. secured-renovacy was the last entry (its per-PKG_MGR `case` branches
// moved into package-managers.md's `iterion:pkgmgr` block, W2.2). A new
// violation on any bot now fails the test outright rather than being excused.
var stackAgnosticExemptions = map[string]string{}

// memVisibilityBotRe detects a per-bot memory declaration in a .bot file.
var memVisibilityBotRe = regexp.MustCompile(`visibility:\s*"bot"`)

// workflowNameRe captures the `workflow NAME:` keyword (column 0).
var workflowNameRe = regexp.MustCompile(`(?m)^workflow\s+([A-Za-z_][A-Za-z0-9_.-]*)\s*:`)

// manifestNameRe captures the top-level `name:` field in manifest.yaml
// (anchored at column 0 so `display_name:` does not match).
var manifestNameRe = regexp.MustCompile(`(?m)^name:\s*"?([A-Za-z0-9_.-]+)"?`)

// TestPerBotMemoryConsumersHaveStableBotID enforces that any catalog bot
// using per-bot memory (visibility: "bot") names its workflow, its bundle
// directory, and its manifest identically.
//
// Why: the per-bot memory tree is keyed by botID, which the runtime takes
// from the `workflow NAME:` keyword on a CLI run (executor.go: botID =
// wf.Name) but from the bundle name on a dispatcher/studio run (runview
// spec.BotID). If those differ, the bot's accumulated vision/decisions
// silently split across two on-disk trees between launch modes and the
// operator's "second session" sees empty memory. Requiring all three
// identical makes botID stable regardless of launch path.
//
// Existing multi-word bots (e.g. feature-dev dir / feature_dev workflow)
// are intentionally exempt: they don't use visibility:bot, so their botID
// divergence is harmless. Only memory consumers are gated.
func TestPerBotMemoryConsumersHaveStableBotID(t *testing.T) {
	bots, _ := teamBotFiles()
	if len(bots) == 0 {
		t.Fatal("no team bots found under bots/*/main.bot")
	}
	for _, botPath := range bots {
		data, err := os.ReadFile(botPath)
		if err != nil {
			t.Errorf("%s: read: %v", botPath, err)
			continue
		}
		if !memVisibilityBotRe.Match(data) {
			continue // not a per-bot memory consumer; botID divergence is harmless
		}
		dir := filepath.Base(filepath.Dir(botPath))

		wf := workflowNameRe.FindSubmatch(data)
		if wf == nil {
			t.Errorf("%s: uses visibility:\"bot\" memory but no `workflow NAME:` found", botPath)
			continue
		}
		wfName := string(wf[1])

		manifestPath := filepath.Join(filepath.Dir(botPath), "manifest.yaml")
		mdata, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Errorf("%s: uses visibility:\"bot\" memory but manifest unreadable: %v", botPath, err)
			continue
		}
		mn := manifestNameRe.FindSubmatch(mdata)
		if mn == nil {
			t.Errorf("%s: manifest has no top-level name:", manifestPath)
			continue
		}
		manifestName := string(mn[1])

		if wfName != dir || manifestName != dir {
			t.Errorf(
				"%s: per-bot memory (visibility:\"bot\") requires workflow name == bundle dir == manifest name so botID is stable across CLI (wf.Name) and dispatcher (bundle name) launches; got workflow=%q dir=%q manifest=%q",
				botPath, wfName, dir, manifestName,
			)
		}
	}
}

func TestCatalogBotsAreStackAgnostic(t *testing.T) {
	teamBots, _ := teamBotFiles()
	demoBots, _ := filepath.Glob("../examples/*/main.bot")
	botPaths := append(teamBots, demoBots...)
	if len(botPaths) == 0 {
		t.Fatal("no catalog bots found under bots/*/main.bot or examples/*/main.bot")
	}

	matched := map[string]bool{} // bot -> had >=1 stack pattern

	for _, botPath := range botPaths {
		bot := filepath.Base(filepath.Dir(botPath))
		data, err := os.ReadFile(botPath)
		if err != nil {
			t.Errorf("%s: read: %v", botPath, err)
			continue
		}
		_, exempt := stackAgnosticExemptions[bot]
		for i, line := range strings.Split(string(data), "\n") {
			for _, p := range stackPatterns {
				if !p.re.MatchString(line) {
					continue
				}
				matched[bot] = true
				if exempt {
					continue
				}
				t.Errorf(
					"%s:%d hardcodes stack-specific logic: %s\n    %s\n  Move it into the bot's skills and dispatch via an adaptive agent + deterministic gate (see CLAUDE.md \"Universal code bots\").",
					botPath, i+1, p.what, strings.TrimSpace(line),
				)
			}
		}
	}

	for bot, reason := range stackAgnosticExemptions {
		if !matched[bot] {
			t.Errorf("bot %q is exempt (%q) but no longer hardcodes any stack pattern — remove it from stackAgnosticExemptions so the test guards it against regression.", bot, reason)
		}
	}
}
