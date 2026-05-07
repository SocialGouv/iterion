package netproxy

// Presets are named rule lists shipped with iterion so workflow
// authors don't have to repeat the LLM-endpoints + package-registries
// + code-hosts boilerplate in every sandbox: block.
//
// Apply a preset by setting `network.preset:` in the .iter file (or
// by passing PresetIterionDefault as a Compile() prefix).
//
// The list is intentionally curated rather than exhaustive — adding a
// new endpoint here is a deliberate API decision. Workflow authors
// who need more can append to `network.rules`.
const (
	// PresetIterionDefault covers the LLM endpoints all claw-supported
	// providers use, the package registries every common runtime
	// relies on (npm, pypi, golang proxy), and the canonical code
	// hosts (github, gitlab, bitbucket). Suitable for the typical
	// "agent installs deps, edits code, opens a PR" workflow.
	PresetIterionDefault = "iterion-default"
)

// PresetRules returns the rule list for a named preset, or (nil,
// false) when the preset is unknown. Callers prepend these rules to
// the workflow's own list so per-workflow `!exclusion` entries can
// override the preset on a host-by-host basis.
func PresetRules(name string) ([]string, bool) {
	switch name {
	case PresetIterionDefault:
		return iterionDefaultRules(), true
	}
	return nil, false
}

func iterionDefaultRules() []string {
	return []string{
		// --- LLM providers ----------------------------------------
		"api.anthropic.com",
		"api.openai.com",
		"openrouter.ai",
		"**.bedrock.amazonaws.com",
		"**.googleapis.com",
		"**.openai.azure.com",
		"api.mistral.ai",

		// --- npm --------------------------------------------------
		"registry.npmjs.org",
		"**.npmjs.org",

		// --- PyPI -------------------------------------------------
		"pypi.org",
		"files.pythonhosted.org",

		// --- Go modules -------------------------------------------
		"proxy.golang.org",
		"sum.golang.org",

		// --- GitHub -----------------------------------------------
		"github.com",
		"**.github.com",
		"codeload.github.com",
		"objects.githubusercontent.com",

		// --- Other code hosts -------------------------------------
		"gitlab.com",
		"**.gitlab.com",
		"bitbucket.org",
		"**.bitbucket.org",

		// --- Linux package mirrors (apt-get inside the container) -
		"deb.debian.org",
		"security.debian.org",

		// --- Nix store / devbox -----------------------------------
		// Required by the iterion-sandbox-{slim,full} images, which
		// ship devbox + Nix; `devbox install` against a workspace
		// devbox.json reaches the Nix binary cache and the upstream
		// Devbox catalog through these hosts.
		"cache.nixos.org",
		"channels.nixos.org",
		"releases.nixos.org",
		"nix-community.cachix.org",
		"devbox.sh",
		"**.devbox.sh",
		"get.jetify.com",
	}
}
