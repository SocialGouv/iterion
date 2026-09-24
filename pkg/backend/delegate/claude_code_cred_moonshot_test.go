package delegate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// --- hint: moonshot -------------------------------------------------

func TestAnthropicCredEnv_HintMoonshotForcesEvenWithAnthropicCtx(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderAnthropic: "sk-anthropic-test",
		secrets.ProviderMoonshot:  "moonshot-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "moonshot", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "moonshot-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want moonshot-test (hint moonshot pins Moonshot routing)", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if got["ANTHROPIC_BASE_URL"] != secrets.MoonshotDefaultBaseURL {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want %q", got["ANTHROPIC_BASE_URL"], secrets.MoonshotDefaultBaseURL)
	}
	if v, present := got["ANTHROPIC_API_KEY"]; !present || v != "" {
		t.Errorf("ANTHROPIC_API_KEY must be present-and-empty on a Moonshot route (an inherited value would ride along): present=%v val=%q", present, v)
	}
}

func TestAnthropicCredEnv_HintMoonshotFallsToEnvKey(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("MOONSHOT_API_KEY", "moonshot-from-env")
	got := anthropicCredEnvForCLI(context.Background(), "moonshot", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "moonshot-from-env" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want the process-env key", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

// The override knob is MOONSHOT_BASE_URL, and ANTHROPIC_BASE_URL is NOT one:
// that variable is z.ai's own documented wiring knob, so on a host configured
// for z.ai it holds z.ai's endpoint. Reading it for a Moonshot key would ship
// the credential to another vendor's gateway — the exact "different provider,
// different bill" failure naming the provider exists to prevent.
func TestAnthropicCredEnv_HintMoonshotIgnoresZaiBaseURLAndHonoursItsOwn(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", secrets.ZAIDefaultBaseURL)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-test"}, nil)

	got := anthropicCredEnvForCLI(ctx, "moonshot", false)
	if got["ANTHROPIC_BASE_URL"] != secrets.MoonshotDefaultBaseURL {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want %q — an ambient z.ai base URL must not retarget a Moonshot key",
			got["ANTHROPIC_BASE_URL"], secrets.MoonshotDefaultBaseURL)
	}

	t.Setenv("MOONSHOT_BASE_URL", "https://api.moonshot.cn/anthropic")
	got = anthropicCredEnvForCLI(ctx, "moonshot", false)
	if got["ANTHROPIC_BASE_URL"] != "https://api.moonshot.cn/anthropic" {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want the MOONSHOT_BASE_URL override", got["ANTHROPIC_BASE_URL"])
	}
}

// THE acceptance test (#1744): with the hint set and no key reachable, the
// node must be REFUSED by name and must never fall through to Anthropic —
// another account, another bill, and a silent one.
//
// Two halves, because either alone is passable with the guard gone:
//   - the env the CLI would receive offers NO Anthropic-flavoured channel,
//     including the ones a container bakes in and the alt-provider switches
//     claw-code-go's detectProvider reads before ANTHROPIC_API_KEY;
//   - the delegate refuses, and the error names the provider and the variable
//     that would have supplied it.
//
// Falsifier (verified, not asserted): make suppressAnthropicWireEnv return
// nil and the whole test reddens — every ambient channel survives into the
// env and no refusal is produced.
func TestAnthropicCredEnv_HintMoonshotNoKeyRefusesAndNeverReachesAnthropic(t *testing.T) {
	for _, sandboxed := range []bool{false, true} {
		name := "host"
		if sandboxed {
			name = "sandboxed"
		}
		t.Run(name, func(t *testing.T) {
			resetClaudeCredEnv(t)
			// Every Anthropic-flavoured credential a parent shell or a baked
			// container env could carry, all hostile at once.
			t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-anthropic")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-oat-ambient")
			t.Setenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oat-ambient-forfait")
			t.Setenv("CLAUDE_CONFIG_DIR", "/iterion/claude-forfait")
			t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
			t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
			t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "1")
			// A run that holds an Anthropic key and a forfait — but not the
			// Moonshot one it was pinned to. The tempting fallback is right
			// there, and must not be taken.
			ctx := ctxWithCreds(t,
				map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ctx-anthropic"},
				map[string]string{string(secrets.OAuthKindClaudeCode): "/tmp/iterion-oauth-claude"},
			)

			got := anthropicCredEnvForCLI(ctx, "moonshot", sandboxed)

			for _, k := range []string{
				"ANTHROPIC_BASE_URL",
				"ANTHROPIC_AUTH_TOKEN",
				"ANTHROPIC_API_KEY",
				"CLAUDE_CODE_OAUTH_TOKEN",
				"CLAUDE_CODE_USE_BEDROCK",
				"CLAUDE_CODE_USE_VERTEX",
				"CLAUDE_CODE_USE_FOUNDRY",
			} {
				v, present := got[k]
				if !present || v != "" {
					t.Errorf("%s must be present-and-empty (suppression signal): present=%v val=%q", k, present, v)
				}
			}
			// CLAUDE_CONFIG_DIR is poisoned rather than cleared: mergeCmdEnv
			// reads an empty value as "absent", and the CLI then defaults to
			// $HOME/.claude — a dev laptop with `claude login` would resolve a
			// valid forfait there.
			cfg, present := got["CLAUDE_CONFIG_DIR"]
			if !present || cfg == "" {
				t.Errorf("CLAUDE_CONFIG_DIR must be present and non-empty (poisoned path): present=%v val=%q", present, cfg)
			}
			if !strings.HasPrefix(cfg, "/") {
				t.Errorf("CLAUDE_CONFIG_DIR must be an absolute path, got %q", cfg)
			}
			if _, err := os.Stat(cfg); err == nil {
				t.Errorf("CLAUDE_CONFIG_DIR poisoned path %q ACTUALLY EXISTS on this host — pick another sentinel", cfg)
			}
			if got[ForfaitSuppressedEnvKey] != "1" {
				t.Errorf("%s must be %q on a suppressed forfait, got %q", ForfaitSuppressedEnvKey, "1", got[ForfaitSuppressedEnvKey])
			}
			// Not just "no key set": the routing label must not read as any
			// flavour of Anthropic service either, or the meter and the
			// session-fork guard would file this run under a credential it
			// never spent.
			if fp := providerFingerprint(got); fp != "anthropic-suppressed" {
				t.Errorf("providerFingerprint = %q, want %q — a suppressed route must not read as a serving one", fp, "anthropic-suppressed")
			}

			// The refusal itself, and what it says.
			err := facadeHintRefusal("moonshot", got)
			if err == nil {
				t.Fatal("facadeHintRefusal returned nil — an unfunded moonshot node must be refused, not spawned against a suppressed env")
			}
			var refusal *ErrNoFacadeCredential
			if !errors.As(err, &refusal) {
				t.Fatalf("refusal is %T, want *ErrNoFacadeCredential so callers can classify it", err)
			}
			if refusal.Provider != "moonshot" {
				t.Errorf("refusal.Provider = %q, want moonshot", refusal.Provider)
			}
			if refusal.EnvVar != "MOONSHOT_API_KEY" {
				t.Errorf("refusal.EnvVar = %q, want MOONSHOT_API_KEY", refusal.EnvVar)
			}
			msg := err.Error()
			for _, want := range []string{"moonshot", "MOONSHOT_API_KEY"} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal message %q does not name %q", msg, want)
				}
			}
		})
	}
}

// A funded node is NOT refused — the guard above must discriminate, not
// simply refuse every moonshot node.
func TestFacadeHintRefusal_FundedNodeIsNotRefused(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-test"}, nil)
	if err := facadeHintRefusal("moonshot", anthropicCredEnvForCLI(ctx, "moonshot", false)); err != nil {
		t.Fatalf("funded moonshot node refused: %v", err)
	}
	// And a node that is not facade-pinned at all keeps the old behaviour:
	// an unfunded `provider: anthropic` node still spawns and lets the CLI's
	// own resolution (and its own error) decide.
	if err := facadeHintRefusal("anthropic", anthropicCredEnvForCLI(context.Background(), "anthropic", false)); err != nil {
		t.Fatalf("non-facade hint refused: %v", err)
	}
}

// The same refusal holds for z.ai — the class, not the site. Written with
// moonshot's because the two branches are now one function, and a test that
// only covered the new provider would let a future edit weaken the old one.
func TestAnthropicCredEnv_HintZAINoKeyIsRefusedByName(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-anthropic")
	err := facadeHintRefusal("zai", anthropicCredEnvForCLI(context.Background(), "zai", false))
	if err == nil {
		t.Fatal("an unfunded zai node must be refused by name too")
	}
	if !strings.Contains(err.Error(), "ZAI_API_KEY") {
		t.Errorf("refusal message %q does not name ZAI_API_KEY", err.Error())
	}
}

// --- default precedence --------------------------------------------

func TestAnthropicCredEnv_AutoMoonshotFromCtxWinsOverAnthropic(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderAnthropic: "sk-anthropic-test",
		secrets.ProviderMoonshot:  "moonshot-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "moonshot-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want moonshot-test", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if got["ANTHROPIC_BASE_URL"] != secrets.MoonshotDefaultBaseURL {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want the Moonshot endpoint", got["ANTHROPIC_BASE_URL"])
	}
}

// The documented asymmetry with ZAI_API_KEY, pinned so it cannot be
// "harmonised" away by accident: MOONSHOT_API_KEY is already the credential
// channel of `backend: "kimi"`, whose CLI reads it from the host env. An
// operator who exported it configured that CLI — it is not a standing
// instruction to reroute every unpinned anthropic-wire node onto Moonshot.
func TestAnthropicCredEnv_AutoIgnoresAmbientMoonshotEnvKey(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("MOONSHOT_API_KEY", "moonshot-for-the-kimi-cli")
	got := anthropicCredEnvForCLI(context.Background(), "", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "" || got["ANTHROPIC_BASE_URL"] != "" {
		t.Errorf("an ambient MOONSHOT_API_KEY must not reroute an unpinned node: got %v", got)
	}
	// ZAI_API_KEY keeps its env fallback — the asymmetry is deliberate and
	// one-sided.
	t.Setenv("ZAI_API_KEY", "zai-from-env")
	got = anthropicCredEnvForCLI(context.Background(), "", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "zai-from-env" {
		t.Errorf("ZAI_API_KEY env fallback lost: got %q", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

// The class guard. secrets.AnthropicWireSlotOrder is read by the usage meter,
// the spend ledger and the sandbox seam; this pins the DELEGATE — the one
// that actually routes — to the same order, by exercising the real function
// on every adjacent pair. A provider inserted in the list but not honoured
// here (or honoured here in another order) reddens.
func TestDefaultPrecedenceFollowsAnthropicWireSlotOrder(t *testing.T) {
	order := secrets.AnthropicWireSlotOrder
	for i := 0; i+1 < len(order); i++ {
		hi, lo := order[i], order[i+1]
		t.Run(hi+"_beats_"+lo, func(t *testing.T) {
			resetClaudeCredEnv(t)
			apiKeys := map[secrets.Provider]string{}
			oauthDirs := map[string]string{}
			for _, slot := range []string{hi, lo} {
				if secrets.OAuthKind(slot).Valid() {
					oauthDirs[slot] = t.TempDir()
					continue
				}
				apiKeys[secrets.Provider(slot)] = "key-for-" + slot
			}
			got := anthropicCredEnvForCLI(ctxWithCreds(t, apiKeys, oauthDirs), "", false)
			if want := envForSlot(t, hi, apiKeys, oauthDirs); !envServes(got, want) {
				t.Errorf("holding %q and %q, the delegate served %v — want the %q credential, the earlier slot of secrets.AnthropicWireSlotOrder",
					hi, lo, got, hi)
			}
		})
	}
}

// envForSlot names the credential value the delegate must be serving for a
// slot, "" for the OAuth dir (which serves a path, checked separately).
func envForSlot(t *testing.T, slot string, apiKeys map[secrets.Provider]string, oauthDirs map[string]string) string {
	t.Helper()
	if secrets.OAuthKind(slot).Valid() {
		return oauthDirs[slot]
	}
	return apiKeys[secrets.Provider(slot)]
}

// envServes reports whether the env map is serving the given credential —
// whichever channel it rides (facade bearer, direct key, or config dir).
func envServes(env map[string]string, want string) bool {
	if want == "" {
		return false
	}
	return env["ANTHROPIC_AUTH_TOKEN"] == want ||
		env["ANTHROPIC_API_KEY"] == want ||
		env["CLAUDE_CONFIG_DIR"] == want
}

// --- the meter's read-back -------------------------------------------

// Two facades on one wire both render as "facade:<url>", so the label alone
// stopped naming a vendor the day the second one landed. The runner charges a
// refusal by this mapping: get it wrong and a Moonshot wall parks the z.ai
// key while the frozen one keeps being handed out.
func TestAnthropicWireFacadeSlot(t *testing.T) {
	resetClaudeCredEnv(t)
	cases := []struct {
		source string
		want   string
	}{
		{providerFingerprint(zaiEnv("k")), string(secrets.ProviderZAI)},
		{providerFingerprint(moonshotEnv("k")), string(secrets.ProviderMoonshot)},
		{PiUsageSourceZAI, string(secrets.ProviderZAI)},
		{PiUsageSourceMoonshot, string(secrets.ProviderMoonshot)},
		{"anthropic-direct", ""},
		{"anthropic-oauth", ""},
		{"", ""},
		{"facade:https://some.operator.proxy/anthropic", ""},
	}
	for _, tc := range cases {
		if got := AnthropicWireFacadeSlot(tc.source); got != tc.want {
			t.Errorf("AnthropicWireFacadeSlot(%q) = %q, want %q", tc.source, got, tc.want)
		}
	}
	// An operator's own MOONSHOT_BASE_URL travels into the label, so the
	// mapping still resolves rather than falling back to a guess.
	t.Setenv("MOONSHOT_BASE_URL", "https://api.moonshot.cn/anthropic")
	if got := AnthropicWireFacadeSlot(providerFingerprint(moonshotEnv("k"))); got != string(secrets.ProviderMoonshot) {
		t.Errorf("with MOONSHOT_BASE_URL overridden, AnthropicWireFacadeSlot = %q, want moonshot", got)
	}
}

// The two facades must stay apart under the base URLs that make them EQUAL —
// asserting it under a cleared env is asserting it in the one configuration
// where no code can get it wrong.
//
// The colliding configuration is not exotic: claw-code-go's own operator hint
// tells a Kimi user to `export ANTHROPIC_BASE_URL=<moonshot endpoint>`, and
// that variable is exactly what zaiEnv reads; one internal gateway (LiteLLM,
// a corporate proxy) in front of both vendors does the same. When both
// rendered one label the meter charged a Moonshot wall to the z.ai
// fingerprint, parking the healthy key and continuing to hand out the walled
// one — the inversion runCredKeys exists to prevent.
func TestAnthropicWireFacadeSlot_ApartUnderCollidingBaseURLs(t *testing.T) {
	for _, tc := range []struct{ name, anthropicBase, moonshotBase string }{
		{"claw_hint_points_anthropic_base_url_at_moonshot", secrets.MoonshotDefaultBaseURL, ""},
		{"moonshot_override_points_at_zai", secrets.ZAIDefaultBaseURL, secrets.ZAIDefaultBaseURL},
		{"one_gateway_in_front_of_both", "https://gateway.internal/anthropic", "https://gateway.internal/anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeCredEnv(t)
			t.Setenv("ANTHROPIC_BASE_URL", tc.anthropicBase)
			t.Setenv("MOONSHOT_BASE_URL", tc.moonshotBase)

			zai, moonshot := zaiEnv("zai-key"), moonshotEnv("moonshot-key")
			// The bench must BITE: if the two routes do not actually share a
			// base URL here, this case proves nothing about collisions.
			if zai["ANTHROPIC_BASE_URL"] != moonshot["ANTHROPIC_BASE_URL"] {
				t.Fatalf("inert case: z.ai routes to %q and Moonshot to %q — no collision to tell apart",
					zai["ANTHROPIC_BASE_URL"], moonshot["ANTHROPIC_BASE_URL"])
			}

			zaiLabel, moonshotLabel := providerFingerprint(zai), providerFingerprint(moonshot)
			if zaiLabel == moonshotLabel {
				t.Fatalf("both facades render %q — the meter cannot tell the two credentials apart", zaiLabel)
			}
			if got := AnthropicWireFacadeSlot(zaiLabel); got != string(secrets.ProviderZAI) {
				t.Errorf("AnthropicWireFacadeSlot(%q) = %q, want zai", zaiLabel, got)
			}
			if got := AnthropicWireFacadeSlot(moonshotLabel); got != string(secrets.ProviderMoonshot) {
				t.Errorf("AnthropicWireFacadeSlot(%q) = %q, want moonshot", moonshotLabel, got)
			}
			// The label rides run records and events: it must carry the
			// routing decision, never the key that paid.
			for _, label := range []string{zaiLabel, moonshotLabel} {
				if strings.Contains(label, "zai-key") || strings.Contains(label, "moonshot-key") {
					t.Errorf("label %q carries the credential", label)
				}
			}
		})
	}
}

func TestUsageMeterBackendForProvider(t *testing.T) {
	for _, p := range []secrets.Provider{secrets.ProviderAnthropic, secrets.ProviderZAI, secrets.ProviderMoonshot} {
		if got := UsageMeterBackendForProvider(p); got != BackendClaudeCode {
			t.Errorf("UsageMeterBackendForProvider(%q) = %q, want %q — anthropic-wire keys are metered by claude_code sessions", p, got, BackendClaudeCode)
		}
	}
	for _, p := range []secrets.Provider{secrets.ProviderOpenAI, secrets.ProviderXAI, secrets.ProviderBedrock, secrets.ProviderVertex} {
		if got := UsageMeterBackendForProvider(p); got != "" {
			t.Errorf("UsageMeterBackendForProvider(%q) = %q, want \"\" — no metered evidence off this wire", p, got)
		}
	}
}
