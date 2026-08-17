package runtime

import (
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// repoDevboxEnabled reports whether the TARGET REPO's `devbox.json` is
// provisioned for this run, resolving the precedence chain the rest of the
// engine uses: CLI/launch override → workflow block → ITERION_REPO_DEVBOX
// → on.
//
// It governs ONE of the two devbox sources. The BOT's own devbox.json is
// never affected: a bot declaring `crane` needs `crane`, and a workflow
// switching this off is saying something about the repo it is pointed at,
// not about itself.
//
// Default ON, because a repo that pins its toolchain usually pins it to be
// built — and a run that builds it needs those tools. The escape hatch is
// for the run that does NOT: a reviewer reads a diff and opens comments,
// and paying its target's full toolchain buys it nothing. On this repo
// that bill is 319 Nix paths / 1.8 GiB unpacked, most of it a desktop GUI
// stack, at every single review.
func (e *Engine) repoDevboxEnabled() bool {
	var wf *ir.Workflow
	if e != nil {
		wf = e.workflow
	}
	return resolveRepoDevbox(e.repoDevboxOverride, wf)
}

// resolveRepoDevbox is the chain itself, split out so both provisioning
// paths (host and sandbox) answer the question identically — the sandbox
// one runs from SandboxParams, with no Engine in reach.
func resolveRepoDevbox(override string, wf *ir.Workflow) bool {
	if v, ok := parseOnOff(override); ok {
		return v
	}
	if wf != nil {
		if v, ok := parseOnOff(wf.RepoDevbox); ok {
			return v
		}
	}
	if v, ok := parseOnOff(os.Getenv("ITERION_REPO_DEVBOX")); ok {
		return v
	}
	return true
}

// parseOnOff reads one layer of an on/off precedence chain. ok=false means
// the layer is unset (or unreadable) and the next one decides. The DSL
// validators reject anything but on|off at their own boundary; the extra
// spellings are accepted from the env, where operators reach for 0/1.
func parseOnOff(v string) (enabled, ok bool) {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "off", "0", "false", "no":
		return false, true
	case "on", "1", "true", "yes":
		return true, true
	}
	return false, false
}

// ValidateRepoDevboxMode rejects a --repo-devbox value that is neither
// empty (inherit) nor on|off. A typo would otherwise read as "inherit" and
// silently keep an install an operator asked to skip.
func ValidateRepoDevboxMode(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "on", "off":
		return nil
	}
	return fmt.Errorf("invalid repo-devbox mode %q: expected on or off", v)
}
