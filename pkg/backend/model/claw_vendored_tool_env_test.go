package model

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The tool half of #1804's derivation (#2802): the in-container runner
// registers claw's own TOOLS from the env that crossed, and those tools read
// the environment under names no provider table carries either — a bash
// budget, a todos path, the config dirs a skill lookup walks. This file is
// the classification of every such name, DERIVED from the vendored claw the
// tool registry imports (pkg/backend/tool imports claw-code-go/pkg/api/tools,
// which forwards to internal/tools), never hand-listed, so a tool claw gains
// arrives classified or red. The walk fails LOUDLY when a package disappears:
// vendor/ is being pruned (ADR-142), and a pruned tool package would
// otherwise shrink this list in silence.
//
// What it does not see, named rather than hidden (same shape as the provider
// half): a name claw builds at run time instead of spelling it, and a name
// read in a claw package OUTSIDE the two tool packages — internal/context's
// SHELL, internal/api's credential reads — of which the provider walk covers
// its own share.
//
// The classes a vendored tool's env name can be in.
const (
	// toolClassUnreachable: iterion's registrars never build the tool that
	// reads the name, so the read never happens for a node. The entry must
	// name the registration that proves it. No name is in this class today.
	toolClassUnreachable = "unreachable"
	// toolClassLauncher: the tool that reads the name is launcher-placed
	// (tool.PlacementLauncher — the web_search shape): the read happens on
	// the launcher and the name never crosses.
	toolClassLauncher = "launcher"
	// toolClassForwarded: the tool runs in the container for a sandboxed
	// node (tool.PlacementSandbox), so the name must cross — as-is for a
	// dial, rewritten to the seeded mount for a credential-bearing dir.
	toolClassForwarded = "forwarded"
	// toolClassUndecided: a Jo-gated question holds the verdict. Until it
	// lands, the name must NOT cross — the fail-closed reading of an open
	// question.
	toolClassUndecided = "undecided"
)

// vendoredToolEnvDecision carries, for every env name claw's tool packages
// read, its class and why. A name the walk finds that is not here fails the
// test; an entry the walk no longer finds fails too.
var vendoredToolEnvDecision = map[string]struct {
	class string
	tool  string // the registered tool whose code path reads the name
	why   string
}{
	// Decided by #1804 with web_search's placement; asserted against the
	// placement table below, never quoted.
	"BRAVE_API_KEY":    {toolClassLauncher, "web_search", "decided by #1804: the search credential is the launcher's"},
	"SEARXNG_URL":      {toolClassLauncher, "web_search", "decided by #1804: the search endpoint is the launcher's"},
	"SEARXNG_ENDPOINT": {toolClassLauncher, "web_search", "decided by #1804: the search endpoint is the launcher's"},
	"CLAW_WEB_SEARCH_BRAVE_URL": {toolClassLauncher, "web_search",
		"same tool, same reason as the #1804 trio: the endpoint override is the launcher's"},
	"CLAW_WEB_SEARCH_DDG_URL": {toolClassLauncher, "web_search",
		"same tool, same reason as the #1804 trio: the endpoint override is the launcher's"},

	"CLAW_TODOS_DIR": {toolClassLauncher, "todo_write",
		"the session todo list lives outside the workspace, keyed on the launcher's own directory — the read happens on the launcher, the name never crosses"},
	"CLAW_BASH_TIMEOUT": {toolClassForwarded, "bash",
		"the deployment's bash budget, spent where bash runs — the container for a sandboxed node; a duration, not a path, so the launcher's value crosses as-is (forwardableToolEnv), the way the ITERION_CLAW_STREAM_* knobs do"},
	"CLAW_CONFIG_HOME": {toolClassForwarded, "skill",
		"the skill lookup reads it in the container; no iterion-seeded mount serves a claw config home, so there is nothing to rewrite to and the launcher's value crosses as-is — resolving where host_state binds the host, skipped where the path does not exist"},
	"CODEX_HOME": {toolClassForwarded, "skill",
		"read in the container by the skill lookup AND by claw's codex auth; it crosses REWRITTEN to the seeded forfait mount (secrets.CodexSandboxConfigDir, applyForfaitAcrossSandbox) or the per-dispatch copy (deliverHostCodexForfait), never as the launcher's path"},
	"CLAUDE_CONFIG_DIR": {toolClassForwarded, "skill",
		"read in the container by the skill lookup AND by claw's claude auth; it crosses REWRITTEN to the seeded forfait mount (secrets.ClaudeCodeSandboxConfigDir, applyForfaitAcrossSandbox), never as the launcher's path"},

	// The pair #2802 leaves to Jo (question 50 of
	// ~/.claude/plans/debt-leap/fleet/questions-jo-20261028.md): launcher-side
	// for ever — computer_use is a launcher-placement tool and a sandboxed
	// node has no display — or forwarded for an opt-in class of
	// desktop-automation runs (a bind-mounted X socket). Until the verdict:
	// the names cross nowhere and the tools stay PlacementRefused for a
	// sandboxed node, which the test below pins.
	"DISPLAY": {toolClassUndecided, "computer_use",
		"UNDECIDED, Jo-gated (#2802, question 50): whether a display ever crosses the sandbox; until then the name does not cross"},
	"WAYLAND_DISPLAY": {toolClassUndecided, "computer_use",
		"UNDECIDED, Jo-gated (#2802, question 50): whether a display ever crosses the sandbox; until then the name does not cross"},
}

// TestEveryVendoredToolEnvNameIsDecided derives the env names claw's tool
// packages read and requires each to be classified with its reason — the
// #2802 extension of TestEveryVendoredProviderEnvNameIsDecided's walk to the
// tool packages.
func TestEveryVendoredToolEnvNameIsDecided(t *testing.T) {
	root := moduleRoot(t)
	dirs := vendoredToolDirs(t, root)
	names := envNamesReadIn(t, dirs)
	if len(names) == 0 {
		t.Fatalf("no env read found in claw's tool packages (%v): the walk no longer matches the vendored source, so a green verdict here proves nothing", dirs)
	}

	for _, name := range names {
		got, ok := vendoredToolEnvDecision[name]
		if !ok {
			t.Errorf("claw's tool packages read %s and nothing decides it: classify it — unreachable (name the registration that proves the tool is never built), launcher (the tool is launcher-placed), forwarded (the tool runs in the container: carry the launcher's value, or rewrite it to the mount the runtime seeded), or undecided with the question that holds the verdict — until then a sandboxed node's tools read a name the crossing never decided (#2802)", name)
			continue
		}
		if strings.TrimSpace(got.why) == "" {
			t.Errorf("%s is classified %s with no reason: the next reader must be able to argue with it", name, got.class)
		}
		// The placement is asserted, never quoted: a tool that moves
		// placement reopens its names' classification.
		placement, why := tool.SandboxPlacementOf(got.tool)
		switch got.class {
		case toolClassLauncher:
			if placement != tool.PlacementLauncher {
				t.Errorf("%s is classified launcher-side but %s is placed %s (%s): the name now has to reach the container, so decide it again, not by omission", name, got.tool, placement, why)
			}
		case toolClassForwarded:
			if placement != tool.PlacementSandbox {
				t.Errorf("%s is classified forwarded but %s is placed %s (%s): nothing in the container reads it any more, or a refused tool does — decide it again", name, got.tool, placement, why)
			}
		case toolClassUndecided:
			if placement != tool.PlacementRefused {
				t.Errorf("%s is UNDECIDED (a Jo-gated question holds it) but %s is placed %s (%s): the undecided reading rests on the tool never executing for a sandboxed node — decide the question, do not let a placement change decide it", name, got.tool, placement, why)
			}
			if !strings.Contains(got.why, "UNDECIDED") {
				t.Errorf("%s is classified undecided without naming the question in its reason", name)
			}
		case toolClassUnreachable:
			// The proof obligation is the entry's `why`: it must name the
			// registration that keeps the reading tool out of every registry.
			if !strings.Contains(got.why, "regist") {
				t.Errorf("%s is classified unreachable without naming the registration that proves it", name)
			}
		default:
			t.Errorf("%s has no class (%q): say unreachable, launcher, forwarded or undecided", name, got.class)
		}
	}
	for name := range vendoredToolEnvDecision {
		if !slices.Contains(names, name) {
			t.Errorf("the classification carries %s, which claw's tool packages no longer read: drop the entry, and the forwarding with it if nothing else needs the name", name)
		}
	}
}

// TestSandboxedClawNodeToolEnvCrossing is the witness for the forwarded
// names: the env the in-container runner RECEIVES carries the launcher's
// bash budget and claw config home, the forfait mounts under their rewritten
// names — and none of the launcher-side or undecided names, however the
// launcher sets them.
func TestSandboxedClawNodeToolEnvCrossing(t *testing.T) {
	t.Setenv("CLAW_BASH_TIMEOUT", "17m")
	t.Setenv("CLAW_CONFIG_HOME", "/launcher/claw-config")
	t.Setenv("CLAW_TODOS_DIR", "/launcher/todos")
	t.Setenv("CODEX_HOME", "/launcher/codex-home")
	t.Setenv("CLAUDE_CONFIG_DIR", "/launcher/claude-config")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	for _, name := range []string{"BRAVE_API_KEY", "SEARXNG_URL", "SEARXNG_ENDPOINT", "CLAW_WEB_SEARCH_BRAVE_URL", "CLAW_WEB_SEARCH_DDG_URL"} {
		t.Setenv(name, "launcher-search-value")
	}

	toolEnv := forwardableToolEnv()
	if got := toolEnv["CLAW_BASH_TIMEOUT"]; got != "17m" {
		t.Errorf("CLAW_BASH_TIMEOUT crossed as %q, want the launcher's %q: a sandboxed node's bash runs on claw's default budget while the host's dial says otherwise", got, "17m")
	}
	if got := toolEnv["CLAW_CONFIG_HOME"]; got != "/launcher/claw-config" {
		t.Errorf("CLAW_CONFIG_HOME crossed as %q, want the launcher's value: the in-container skill lookup otherwise walks other roots than the host's, with nothing said", got)
	}
	for _, name := range []string{"CLAW_TODOS_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "DISPLAY", "WAYLAND_DISPLAY",
		"BRAVE_API_KEY", "SEARXNG_URL", "SEARXNG_ENDPOINT", "CLAW_WEB_SEARCH_BRAVE_URL", "CLAW_WEB_SEARCH_DDG_URL"} {
		if got, ok := toolEnv[name]; ok {
			t.Errorf("%s crossed through forwardableToolEnv as %q: its classification (%s) says it must not — decide it again, here and there", name, got, vendoredToolEnvDecision[name].class)
		}
	}

	// The credential-bearing config dirs never cross as the launcher's path:
	// without a resolved forfait they are absent, with one they point at the
	// seeded in-sandbox mount.
	env := envFor(t, context.Background())
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if got, ok := env[name]; ok {
			t.Errorf("%s crossed as %q with no resolved forfait: the runner must never read the launcher's path — a path that does not exist in the container, or worse, names the host's credential dir under host_state's bind", name, got)
		}
	}
	codexForfait := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindCodex): "/host/tmp/whatever"},
	})
	if got := envFor(t, codexForfait)["CODEX_HOME"]; got != secrets.CodexSandboxConfigDir {
		t.Errorf("CODEX_HOME = %q with the run's codex forfait, want the seeded in-sandbox dir %q (never the launcher's %q)", got, secrets.CodexSandboxConfigDir, "/launcher/codex-home")
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-PLATFORM")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ZAI_API_KEY", "")
	claudeForfait := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	if got := envFor(t, claudeForfait)["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q with the run's claude forfait, want the seeded in-sandbox dir %q (never the launcher's %q)", got, secrets.ClaudeCodeSandboxConfigDir, "/launcher/claude-config")
	}
}

// vendoredToolDirs derives, from the imports of iterion's tool package, the
// vendored claw tool directories whose env reads a node's tools perform: the
// re-exporting package pkg/backend/tool imports, and the internal package it
// forwards to. A rename or a prune fails loudly rather than shrinking the
// derivation — the hazard TestEveryVendoredProviderEnvNameIsDecided names for
// the provider half.
func vendoredToolDirs(t *testing.T, root string) []string {
	t.Helper()
	const clawPrefix = "github.com/SocialGouv/claw-code-go/"
	const apiTools = clawPrefix + "pkg/api/tools"
	const internalTools = clawPrefix + "internal/tools"

	iterionToolDir := filepath.Join(root, "pkg", "backend", "tool")
	var imports []string
	for _, path := range nonTestGoFilesIn(t, iterionToolDir) {
		imports = append(imports, importsOfFile(t, path)...)
	}
	if !slices.Contains(imports, apiTools) {
		t.Fatalf("pkg/backend/tool no longer imports %s: the registry's tools come from elsewhere now — re-point this derivation at the packages that serve them, or every tool env name is unguarded", apiTools)
	}
	apiToolsDir := filepath.Join(root, "vendor", filepath.FromSlash(apiTools))
	if !slices.Contains(importsOfDir(t, apiToolsDir), internalTools) {
		t.Fatalf("%s no longer imports %s: the tool implementations moved — re-point this derivation, or the names they read are unguarded", apiTools, internalTools)
	}
	dirs := []string{apiToolsDir, filepath.Join(root, "vendor", filepath.FromSlash(internalTools))}
	for _, dir := range dirs {
		if names := envNamesReadIn(t, []string{dir}); len(names) == 0 && strings.HasSuffix(dir, "internal/tools") {
			t.Fatalf("no env read found in %s: a prune took the package's reads, and with them every classification here — decide the names it used to read before pruning it", dir)
		}
	}
	return dirs
}
