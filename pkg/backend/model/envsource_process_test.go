package model

// The serial half of the env seam's witnesses (#2808): seeding the
// PROCESS environment is global state — the parallel campaign's table
// (parallel_campaign_test.go) owns this file, and nothing here calls
// t.Parallel. What only a process seed can prove: a carried environment
// is hermetic AGAINST the process (the fallback mutation — reading
// os.Getenv for a name the map does not hold — turns the first witness
// red), and the process stays the default when nothing is carried.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A name seeded in the process does NOT read through a carried
// environment that does not hold it.
func TestEnvSeamIsHermeticAgainstTheProcess(t *testing.T) {
	name := fmt.Sprintf("ITERION_ENVSEAM_WITNESS_%d", os.Getpid())
	if err := os.Setenv(name, "process-value"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(name) })

	ctx := withCarriedEnv(context.Background(), carriedEnv{"ITERION_ENVSEAM_OTHER": "x"})
	if got := envGetenv(ctx, name); got != "" {
		t.Errorf("the process-seeded %s read %q through the carried environment: a carried test saw the process", name, got)
	}
	if _, ok := envLookupEnv(ctx, name); ok {
		t.Errorf("envLookupEnv(%s) = planted through the carried environment: the process leaked", name)
	}
	for _, pair := range envEnviron(ctx) {
		if pair == name+"=process-value" {
			t.Errorf("envEnviron carried %s: the process leaked into the whole-env read", pair)
		}
	}
}

// The process stays the default: nothing carried, the process answers.
func TestEnvSeamDefaultsToTheProcess(t *testing.T) {
	name := fmt.Sprintf("ITERION_ENVSEAM_DEFAULT_%d", os.Getpid())
	t.Setenv(name, "from-process")
	if got := envGetenv(context.Background(), name); got != "from-process" {
		t.Errorf("nothing carried: envGetenv = %q, want the process value", got)
	}
	if v, ok := envLookupEnv(context.Background(), name); !ok || v != "from-process" {
		t.Errorf("nothing carried: envLookupEnv = %q,%v, want the process value", v, ok)
	}
	// A nil carried environment is a no-op, not an empty one.
	if got := envGetenv(withCarriedEnv(context.Background(), nil), name); got != "from-process" {
		t.Errorf("nil carried env: envGetenv = %q, want the process value", got)
	}
}

// The correction round's witnesses for the migrated readers whose
// observation needs the process seeded or a package global installed —
// serial, like every process touch in this file (#2808).
//
// Coverage declaration for the readers the round asked about:
// hostCodexForfait, resolveAnthropicDirect and anthropicFromCtxForfait are
// witnessed below. ResolveWithContext's GLM wire (registry.go: the
// glmBaseURL(envGetenv(ctx, "ANTHROPIC_BASE_URL")) line) is NOT, by
// refusal: its read's product lands in an opaque api.APIClient with no
// accessor, and every negative path out of that branch consults the
// machine's disk forfait, so no deterministic carried-env witness exists;
// the gate that decides whether the branch runs at all
// (glmKeyReachableUnderHint) and the same name's forfait-side read
// (anthropicFromCtxForfait, below) are witnessed.

// hostCodexForfait's gate reads the switch through the seam: the carried
// "1" ships the (forged) host forfait; a carried env that does not hold
// the switch passes nothing, whatever the process holds. Mutant — the
// gate reading the process: the first half reddens on hosts without the
// switch set, the second on hosts with it.
func TestHostCodexForfaitReadsTheSwitchThroughTheCarriedEnv(t *testing.T) {
	t.Setenv("CODEX_HOME", writeCodexAuth(t))
	payload, refused := hostCodexForfait(withCarriedEnv(context.Background(), carriedEnv{"ITERION_OPENAI_USE_OAUTH": "1"}), "openai/gpt-6")
	if refused != "" || len(payload) == 0 {
		t.Errorf("carried ITERION_OPENAI_USE_OAUTH=1: payload %d bytes, refused %q — the switch was not read through the seam", len(payload), refused)
	}
	payload, refused = hostCodexForfait(withCarriedEnv(context.Background(), carriedEnv{"CODEX_HOME": "irrelevant"}), "openai/gpt-6")
	if payload != nil || refused != "" {
		t.Errorf("a carried env without the switch: payload %d bytes, refused %q — the process leaked", len(payload), refused)
	}
}

// resolveAnthropicDirect's ambient-key read, with the subscription forfait
// forbidden so every fallback errors: the carried key is the only way out
// without an error, on every machine. Mutant — the site reading the
// process: one side or the other reddens whether or not the process holds
// ANTHROPIC_API_KEY.
func TestResolveAnthropicDirectReadsTheKeyThroughTheCarriedEnv(t *testing.T) {
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	r := NewRegistry()
	client, err := r.resolveAnthropicDirect(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_API_KEY": "sk-ant-carried"}), "claude-haiku-4-5")
	if err != nil || client == nil {
		t.Errorf("carried ANTHROPIC_API_KEY: client %v, err %v — the key was not read through the seam", client, err)
	}
	if client, err := r.resolveAnthropicDirect(withCarriedEnv(context.Background(), carriedEnv{"OPENAI_API_KEY": "sk-other"}), "claude-haiku-4-5"); err == nil || client != nil {
		t.Errorf("a carried env without the key: client %v, err nil — the process leaked", client)
	}
}

// anthropicFromCtxForfait's wire read: a carried z.ai base URL refuses the
// run's forfait (AnthropicForfaitWireOK) before the dir is consulted; a
// carried env without the name resolves the default wire and the forfait
// serves. Mutant — the site reading the process: reddens whatever
// ANTHROPIC_BASE_URL the process holds.
func TestAnthropicFromCtxForfaitReadsTheWireThroughTheCarriedEnv(t *testing.T) {
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "0")
	dir := forfaitDirForTest(t)
	t.Cleanup(SetOAuthDirLookup(func(ctx context.Context) (func(string) string, bool) {
		return func(kind string) string {
			if kind == string(secrets.OAuthKindClaudeCode) {
				return dir
			}
			return ""
		}, true
	}))
	r := NewRegistry()
	if _, ok, err := r.anthropicFromCtxForfait(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic"}), "claude-haiku-4-5"); ok || err != nil {
		t.Errorf("carried z.ai wire: ok=%v err=%v, want the forfait refused (ok=false)", ok, err)
	}
	if _, ok, err := r.anthropicFromCtxForfait(withCarriedEnv(context.Background(), carriedEnv{"ANTHROPIC_API_KEY": "sk-other"}), "claude-haiku-4-5"); err != nil || !ok {
		t.Errorf("carried env without ANTHROPIC_BASE_URL: ok=%v err=%v, want the forfait serving", ok, err)
	}
}
