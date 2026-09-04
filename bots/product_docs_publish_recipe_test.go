package bots

import (
	"os"
	"strings"
	"testing"
)

// The publish recipe is prose an agent follows with a registry-write token in
// hand, so its credential-lifecycle rules are load-bearing — and prose is
// exactly what a later tightening edit drops without noticing. These are
// ANTI-DELETION guards, not semantic proofs: a grep cannot tell a rule from a
// quotation of one. What it catches is the likelier regression — a reflow that
// loses a guard. Reword deliberately and update the literal here in the same
// change; the failure message names what was lost and why it was written.

const publishRecipePath = "product-docs/skills/publish-static-site.md"

func publishRecipe(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(publishRecipePath)
	if err != nil {
		t.Fatalf("read %s: %v", publishRecipePath, err)
	}
	return string(src)
}

// TestPublishRecipeScopesRegistryLogin guards the fix for the credential the
// recipe used to leave behind. `crane auth login` PERSISTS through Docker's
// config store — `$HOME/.docker/config.json` unless DOCKER_CONFIG says
// otherwise. Under a sandbox that dies with the container, but this bot
// declares no `sandbox:` block, so `sandbox: auto` degrades to a host run
// whenever the host cannot sandbox — and there the login lands in the
// operator's real home, leaving a reusable `write:packages` token behind and
// breaking the bot's own hard rule ("do NOT modify anything outside your
// scratch build directory", bots/product-docs/main.bot).
func TestPublishRecipeScopesRegistryLogin(t *testing.T) {
	src := publishRecipe(t)

	// Anchor on the COMMAND, not on the word: the surrounding prose names
	// `crane auth login` too, and the ordering check below must measure the
	// invocation.
	const loginCmd = `crane auth login "$REGISTRY_HOST"`
	const scopeExport = `export DOCKER_CONFIG="$SCRATCH/.docker"`
	login := strings.Index(src, loginCmd)
	if login < 0 {
		t.Skipf("%s: the recipe no longer logs in with crane — this guard has nothing "+
			"to hold", publishRecipePath)
	}
	scope := strings.Index(src, scopeExport)
	if scope < 0 {
		t.Fatalf("%s: the recipe runs `crane auth login` without pointing DOCKER_CONFIG "+
			"at the scratch directory first — on a host run that persists the registry "+
			"token into the operator's ~/.docker/config.json, outside the only directory "+
			"this bot may write to", publishRecipePath)
	}
	// The login must be scoped BEFORE it happens: an export that follows it
	// confines nothing.
	if scope > login {
		t.Fatalf("%s: DOCKER_CONFIG is scoped AFTER the login — the token is already "+
			"written to the default store by then", publishRecipePath)
	}
	if !strings.Contains(src, `rm -rf "$DOCKER_CONFIG"`) {
		t.Fatalf("%s: nothing removes the scratch docker config — the scoped login "+
			"still outlives the push", publishRecipePath)
	}
}
