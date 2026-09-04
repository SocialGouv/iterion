package bots

import (
	"os"
	"os/exec"
	"regexp"
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

// TestPublishRecipeDoesNotTrustSecretEnvVars guards the second half of the
// credential contract: a file secret's `env:` reaches the process environment
// through exactly one path — pkg/runtime/sandbox_secret_files.go writes it into
// the SANDBOX spec. On a host run the only per-run env addition a delegated
// claude_code spawn gets is the devbox PATH (pkg/runtime/devbox_host.go), so
// `$REGISTRY_TOKEN` is unset. A recipe that redirects from it under `set -e`
// dies on an unopenable path, and the "Known refusals" section then teaches the
// agent to report a credential problem that does not exist.
func TestPublishRecipeDoesNotTrustSecretEnvVars(t *testing.T) {
	src := publishRecipe(t)

	const loginCmd = `crane auth login "$REGISTRY_HOST"`
	login := strings.Index(src, loginCmd)
	if login < 0 {
		t.Skipf("%s: the recipe no longer logs in with crane — this guard has nothing "+
			"to hold", publishRecipePath)
	}
	line := src[login:]
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if strings.Contains(line, `"$REGISTRY_TOKEN"`) {
		t.Fatalf("%s: the login reads from $REGISTRY_TOKEN directly — that variable is "+
			"injected only under a sandbox, so on a host run this redirect fails and the "+
			"agent misreports it as a rejected token. Redirect from the path pinned with "+
			"the ${REGISTRY_TOKEN:-<rendered path>} fallback instead.\n  %s",
			publishRecipePath, line)
	}
	if !strings.Contains(src, `REGISTRY_TOKEN_PATH="${REGISTRY_TOKEN:-`) {
		t.Fatalf("%s: the recipe has no fallback from $REGISTRY_TOKEN to the path the "+
			"workflow renders — a host run then has no way to reach the token",
			publishRecipePath)
	}
}

// TestPublishRecipeHandsOffADigest guards the rollout property. The image's
// content is the docs PLUS the converter (TOOLS_REF) PLUS the floating
// nginx-unprivileged base, while the tag is only the docs commit — so the same
// tag can carry different bytes. A deploy target handed that tag sees an
// unchanged pod spec, rolls nothing out, and keeps serving the previously
// pulled image; `verify_publish` asserts a 200 with a title, so it cannot tell
// that apart from a fresh site. Only a digest reference forces the rollout.
func TestPublishRecipeHandsOffADigest(t *testing.T) {
	src := publishRecipe(t)

	if !strings.Contains(src, `DIGEST="$(crane digest "${IMAGE}:${TAG}")"`) {
		t.Fatalf("%s: the recipe never resolves the pushed manifest digest, so it has "+
			"no immutable reference to hand the deploy target", publishRecipePath)
	}
	_, handoff, found := strings.Cut(src, "## 3. Hand-off to the deploy-target skill")
	if !found {
		t.Fatalf("%s: no hand-off section — the deploy target is told nothing about "+
			"which reference to run", publishRecipePath)
	}
	if i := strings.Index(handoff, "\n## "); i >= 0 {
		handoff = handoff[:i]
	}
	if !strings.Contains(handoff, `IMAGE="${IMAGE_REF}"`) {
		t.Fatalf("%s: the hand-off does not give the deploy target the digest "+
			"reference ${IMAGE_REF}", publishRecipePath)
	}
	if strings.Contains(handoff, `IMAGE="${IMAGE}:${TAG}"`) {
		t.Fatalf("%s: the hand-off gives the deploy target the mutable :${TAG} alias — "+
			"an unchanged tag is an unchanged pod spec, so a rebuilt site never rolls "+
			"out and the truth gate still reports success", publishRecipePath)
	}
}

var shBlock = regexp.MustCompile("(?s)```sh\n(.*?)```")

// TestPublishRecipeShellBlocksParse runs `sh -n` over every shell block in the
// recipe. This prose is EXECUTABLE — the agent pastes it into a shell holding a
// registry-write token — so a stray quote or an unbalanced brace does not
// degrade the run, it kills the publish pass under `set -e` after the build has
// already been paid for. POSIX `sh` is the strict check: the block runs in the
// agent's bash, but nothing in it needs a bashism.
func TestPublishRecipeShellBlocksParse(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	blocks := shBlock.FindAllStringSubmatch(publishRecipe(t), -1)
	if len(blocks) == 0 {
		t.Fatalf("%s: no ```sh block found — the recipe's commands are what the "+
			"agent actually runs, and this guard is now vacuous", publishRecipePath)
	}
	for i, m := range blocks {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(m[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: shell block %d does not parse (%v): %s\n%s",
				publishRecipePath, i, err, out, m[1])
		}
	}
}

const productDocsBotPath = "product-docs/main.bot"

// publishSystemPrompt returns the body of `prompt publish_system:` — up to the
// next top-level declaration, which is the only reliable terminator (a blank
// line is not: the prompt has several).
func publishSystemPrompt(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(productDocsBotPath)
	if err != nil {
		t.Fatalf("read %s: %v", productDocsBotPath, err)
	}
	_, after, found := strings.Cut(string(src), "\nprompt publish_system:\n")
	if !found {
		t.Fatalf("%s: no `prompt publish_system:` block — the publish agent's operating "+
			"rules have moved and these guards no longer measure anything",
			productDocsBotPath)
	}
	// Fail rather than fall back to "the rest of the file": a guard that
	// silently widens its scope keeps passing on prose that has moved out of
	// the block it is supposed to measure.
	i := regexp.MustCompile(`(?m)^(prompt|schema|agent|tool|judge|cursor|workflow) `).
		FindStringIndex(after)
	if i == nil {
		t.Fatalf("%s: no top-level declaration after `prompt publish_system:` — the "+
			"block's end cannot be located, so these guards would grep the whole file",
			productDocsBotPath)
	}
	return after[:i[0]]
}

// TestPublishSystemKeepsItsSafetyClauses is an anti-deletion guard on the two
// clauses in publish_system that a reword would silently drop, both of which
// this bot pays for in credentials.
//
// It is not a semantic proof — a grep cannot tell a rule from a quotation of
// one. It catches the likelier regression: someone tightening the prompt and
// losing a guard with it. If you reword deliberately, update the literal here
// in the same change.
func TestPublishSystemKeepsItsSafetyClauses(t *testing.T) {
	prompt := publishSystemPrompt(t)

	// 1. STOP rather than improvise. `skills: ["deploy-target"]` is soft on both
	// ends (pkg/dsl/ir/validate_skills.go warns on the NAME only;
	// pkg/runtime/library_skills.go skips an unresolvable ref with a log line),
	// so nothing but this clause and publish_gate stands between an unattached
	// playbook and an agent improvising a deployment with a registry-write token
	// and a cluster credential.
	for _, want := range []string{
		".claude/skills/deploy-target/SKILL.md",
		".claude/skills/deploy-target.md",
		"NEVER improvise a platform",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("%s: publish_system no longer carries %q — without it an unattached "+
				"deploy-target reaches a privileged agent as nothing but a start-up warning",
				productDocsBotPath, want)
		}
	}

	// 2. The credential PATH is the contract. A file secret's `env:` is written
	// into the SANDBOX spec only (pkg/runtime/sandbox_secret_files.go), so on a
	// host run $DEPLOY_CREDENTIAL and $REGISTRY_TOKEN are unset. A prompt that
	// names only the variables teaches the agent to report a missing credential
	// that is mounted and readable.
	for _, want := range []string{
		"{{secrets.deploy_credential.path}}",
		"{{secrets.registry_token.path}}",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("%s: publish_system no longer renders %s — the agent is left with an "+
				"env var that only a sandboxed run sets", productDocsBotPath, want)
		}
	}
	if !strings.Contains(prompt, "unset variable is therefore NOT a missing credential") {
		t.Errorf("%s: publish_system no longer says an unset $DEPLOY_CREDENTIAL / "+
			"$REGISTRY_TOKEN is not a missing credential — on a host run that is exactly "+
			"the wrong conclusion, and the bot reports a false remedy", productDocsBotPath)
	}
}
