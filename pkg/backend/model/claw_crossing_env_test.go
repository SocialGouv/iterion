package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The fork lane's container env (ADR-162 decision 7): an untrusted run's
// container holds its credentials as Layer 2 placeholders and holds NO value
// the egress proxy cannot substitute — the pod's env crosses as the explicit
// empty, the forfait's mounts ship stripped. Every witness drives the REAL
// crossing (forwardableProviderEnv, and resolveClawCall for the strip) under
// the trust the spawn stamps, each with a trusted control: the trusted value
// is what crossed before the floor existed.

// forkEnv is the crossing a fork-lane spawn composes: the run's credentials
// under the trust executeViaSandboxRunner stamps on the ctx.
func forkEnv(t *testing.T, creds secrets.Credentials, model string) map[string]string {
	t.Helper()
	ctx := credroute.WithRunTrust(secrets.WithCredentials(context.Background(), creds), store.RunTrustFork)
	env, _, err := forwardableProviderEnv(ctx, model, "")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	return env
}

// A run key crosses as its placeholder: the value never reaches the
// container, and what does is the token the egress proxy substitutes toward
// the credential's own host. Mutant — the crossing handing the raw value:
// the container would hold the key.
func TestTheForkLaneCrossesItsRunKeysAsPlaceholders(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-pod-openai-witness")
	key := "sk-ant-fork-run-key-witness"
	creds := secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: key}}

	if env := envFor(t, secrets.WithCredentials(context.Background(), creds)); env["ANTHROPIC_API_KEY"] != key {
		t.Fatalf("trusted control: ANTHROPIC_API_KEY = %q, want the raw key — the trusted crossing is today's", env["ANTHROPIC_API_KEY"])
	}
	if env := envFor(t, secrets.WithCredentials(context.Background(), creds)); env["OPENAI_API_KEY"] != "sk-pod-openai-witness" {
		t.Fatalf("trusted control: OPENAI_API_KEY = %q, want the pod's — the unstamped crossing is today's", env["OPENAI_API_KEY"])
	}
	env := forkEnv(t, creds, "anthropic/claude-sonnet-4")
	got := env["ANTHROPIC_API_KEY"]
	if got != secrets.CredentialPlaceholder(key) {
		t.Fatalf("fork: ANTHROPIC_API_KEY = %q, want the key's Layer 2 placeholder", got)
	}
	if got == key || !secrets.IsCredentialPlaceholder(got) {
		t.Fatalf("fork: ANTHROPIC_API_KEY = %q — the raw value crossed, or the token is not a placeholder", got)
	}
	// The pod's own key never rides beside it: what the floor withheld
	// crosses as the EXPLICIT EMPTY (present, not absent), so no container
	// env or workspace .env auto-load can refill the deployment's money.
	pod, present := env["OPENAI_API_KEY"]
	if !present || pod != "" {
		t.Fatalf("fork: OPENAI_API_KEY = %q (present=%v), want the explicit empty — the launcher's env does not reach an untrusted container", pod, present)
	}
}

// A pinned key and a gateway pair cross the same way: the pin as its
// placeholder on the node that names its provider, the pair's KEY as its
// placeholder beside the plain URL it was sealed for (a URL is not a
// credential).
func TestTheForkLaneCrossesItsPinsAndPairsAsPlaceholders(t *testing.T) {
	pin := "moonshot-fork-pinned-witness"
	env := forkEnv(t, secrets.Credentials{
		PinnedAPIKeys:     map[secrets.Provider]string{secrets.ProviderMoonshot: pin},
		PinnedAPIBaseURLs: map[secrets.Provider]string{},
	}, "moonshot/kimi-k2")
	if got := env["MOONSHOT_API_KEY"]; got != secrets.CredentialPlaceholder(pin) {
		t.Fatalf("fork pin: MOONSHOT_API_KEY = %q, want the pin's placeholder", got)
	}

	pairKey, pairURL := "gw-fork-pair-key-witness", "https://llm-gw.example.com"
	env = forkEnv(t, secrets.Credentials{
		APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: pairKey},
		APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: pairURL},
	}, "openai_compatible/no-table-has-this-id")
	if got := env[compatgw.APIKeyEnv]; got != secrets.CredentialPlaceholder(pairKey) {
		t.Fatalf("fork pair: %s = %q, want the pair key's placeholder", compatgw.APIKeyEnv, got)
	}
	if got := env[compatgw.BaseURLEnv]; got != pairURL {
		t.Fatalf("fork pair: %s = %q, want the sealed URL — a URL is not a credential", compatgw.BaseURLEnv, got)
	}
}

// A key whose provider rides an SDK signing chain names no host the proxy
// could substitute toward, so the fork lane refuses the leg typed — the
// container would hold a placeholder that resolves nowhere (the landing
// floor's ChannelRunKey branch). Mutant — the branch dropped: the crossing
// ships a dead token and the node dies mid-run on an opaque 401.
func TestTheForkLaneRefusesAKeyThatRidesASigningChain(t *testing.T) {
	ctx := credroute.WithRunTrust(secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAzure: "azure-fork-run-key-witness"},
	}), store.RunTrustFork)
	_, err := resolveClawCall(ctx, NewRegistry(), credroute.Call{
		Backend: "claw", Spec: "foundry/gpt-5.5", Leg: credroute.Leg{}, Role: credroute.RoleNode,
		Sandbox: credroute.Sandbox{Contained: true},
	})
	var refused *credroute.ErrCrossingRefused
	if !errors.As(err, &refused) {
		t.Fatalf("fork leg on a foundry node = %v, want the typed crossing refusal", err)
	}
	if !strings.Contains(refused.Channel, "azure") || !strings.Contains(refused.Because, "signing chain") {
		t.Fatalf("the refusal must name the provider and the signing chain: channel=%q because=%q", refused.Channel, refused.Because)
	}
	// Trusted control: the same key funds the node exactly as today.
	if _, err := resolveClawCall(secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAzure: "azure-fork-run-key-witness"},
	}), NewRegistry(), credroute.Call{
		Backend: "claw", Spec: "foundry/gpt-5.5", Leg: credroute.Leg{}, Role: credroute.RoleNode,
		Sandbox: credroute.Sandbox{Contained: true},
	}); err != nil {
		t.Fatalf("trusted control: the foundry leg its key funds = %v, want today's crossing", err)
	}
}

// A crossing the fork lane admits ships no forfait mount: the run's ChatGPT
// forfait composes into the crossing's env so the landing can name it, but a
// crossing its own key funds keeps none of it — CODEX_HOME crosses as the
// explicit empty, CLAUDE_CONFIG_DIR with it, the OAuth-forcing switch is
// deleted (stripForfaitCrossing): the container holds placeholder tokens
// only. Mutant — the strip dropped: the mount crosses and the OAuth refresh
// flow the egress proxy cannot hold rides it.
func TestTheForkLaneShipsNoForfaitMountItAdmits(t *testing.T) {
	creds := secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-fork-run-key-witness"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindCodex): t.TempDir()},
	}
	call := credroute.Call{
		Backend: "claw", Spec: "anthropic/claude-sonnet-4", Leg: credroute.Leg{}, Role: credroute.RoleNode,
		Sandbox: credroute.Sandbox{Contained: true},
	}

	res, err := resolveClawCall(secrets.WithCredentials(context.Background(), creds), NewRegistry(), call)
	if err != nil {
		t.Fatalf("trusted control: %v", err)
	}
	if got := res.env["CODEX_HOME"]; got != secrets.CodexSandboxConfigDir {
		t.Fatalf("trusted control: CODEX_HOME = %q, want %q — the trusted crossing ships the forfait mount today's does", got, secrets.CodexSandboxConfigDir)
	}
	if res.env["ITERION_OPENAI_USE_OAUTH"] != "1" {
		t.Fatalf("trusted control: ITERION_OPENAI_USE_OAUTH = %q, want 1 — without it the mount proves nothing", res.env["ITERION_OPENAI_USE_OAUTH"])
	}

	res, err = resolveClawCall(credroute.WithRunTrust(secrets.WithCredentials(context.Background(), creds), store.RunTrustFork), NewRegistry(), call)
	if err != nil {
		t.Fatalf("the fork leg its run key funds = %v, want the crossing admitted", err)
	}
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		got, present := res.env[name]
		if !present || got != "" {
			t.Fatalf("%s = %q (present=%v), want the explicit empty — no forfait mount crosses an untrusted run", name, got, present)
		}
	}
	if v, present := res.env["ITERION_OPENAI_USE_OAUTH"]; present {
		t.Fatalf("ITERION_OPENAI_USE_OAUTH = %q crossed — the OAuth-forcing switch must be deleted, not emptied", v)
	}
}
