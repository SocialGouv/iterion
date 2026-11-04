package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// sandboxedClawSpend is the resolver's answer for a sandboxed claw leg on
// spec under hint — the admission's question, and the dispatch's crossing.
func sandboxedClawSpend(ctx context.Context, spec, hint string, home credroute.SandboxHomeState) (credroute.Spend, error) {
	return credroute.Resolve(ctx, credroute.Call{
		Backend: delegate.BackendClaw, Spec: spec, Leg: credroute.Leg{Provider: hint},
		Role: credroute.RoleNode, Sandbox: credroute.Sandbox{Contained: true, Home: home},
	})
}

// podCredentialEnvVars are the credentials of the pod a sandboxed claw node's
// container could authenticate with, written out by hand rather than derived
// from the predicate under test (stampWithheldEnvName).
var podCredentialEnvVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY",
	"OPENAI_API_KEY", "AZURE_OPENAI_API_KEY", "MOONSHOT_API_KEY", "XAI_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "GROQ_API_KEY", "DEEPSEEK_API_KEY", "MISTRAL_API_KEY",
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
}

// stampBench gives the host a value under every forwarded name — a
// credential of the pod under each podCredentialEnvVars name, an operator
// setting under the rest — and a valid env gateway, and returns the pod's
// value for a name.
func stampBench(t *testing.T) func(name string) string {
	t.Helper()
	// The list must cover every channel the container reads a credential
	// from, or the witness below would pass over the one it forgot.
	for _, name := range byokEnvVar {
		if !slices.Contains(podCredentialEnvVars, name) {
			t.Fatalf("inert bench: %s is a key the container reads (byokEnvVar) and the pod list misses it", name)
		}
	}
	for _, name := range clawSandboxCredentialEnvKeys {
		if slices.Contains(providerCredentialEnvVars, name) && !slices.Contains(podCredentialEnvVars, name) {
			t.Fatalf("inert bench: %s is a forwarded credential channel (clawSandboxCredentialEnvKeys) and the pod list misses it", name)
		}
	}
	pod := func(name string) string { return "pod-" + strings.ToLower(name) }
	for _, name := range providerCredentialEnvVars {
		t.Setenv(name, "")
	}
	for _, name := range podCredentialEnvVars {
		if !slices.Contains(providerCredentialEnvVars, name) {
			t.Fatalf("inert bench: %s is not forwarded at all, so withholding it proves nothing", name)
		}
		t.Setenv(name, pod(name))
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example/anthropic")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	t.Setenv("ITERION_LLM_USER_AGENT", "operator-agent")
	t.Setenv("ITERION_CODEX_VERSION", "0.0.0-bench")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://93.184.216.34/v1")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "pod-env-gateway")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
	return pod
}

// A stamped run's crossing carries no credential of the pod: each crosses as
// the explicit empty, so neither the pod's value nor one the container
// carries under that name funds the node, while the run's own key for the
// node crosses and the operator's settings that authenticate nothing — a
// routing name, a client identity, the subscription refusal — still do. The
// unstamped twin on the same pod carries the pod's credentials, so the stamp
// alone decides.
func TestTheStampedCrossingCarriesNothingOfThePod(t *testing.T) {
	pod := stampBench(t)
	const spec = "anthropic/claude-sonnet-4"
	runKey := map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"}

	twin, _, err := forwardableProviderEnv(secrets.WithCredentials(context.Background(), secrets.Credentials{APIKeys: runKey}), spec, "")
	if err != nil {
		t.Fatalf("inert bench: the unstamped crossing: %v", err)
	}
	if got := twin["ANTHROPIC_AUTH_TOKEN"]; got != pod("ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("inert bench: the unstamped crossing carries ANTHROPIC_AUTH_TOKEN=%q, want the pod's — the stamp would decide nothing", got)
	}

	env, _, err := forwardableProviderEnv(secrets.WithCredentials(context.Background(), secrets.Credentials{
		AmbientRefused: true,
		APIKeys:        runKey,
	}), spec, "")
	if err != nil {
		t.Fatalf("a stamped node its run funds was refused: %v", err)
	}
	if got := env["ANTHROPIC_API_KEY"]; got != "sk-run" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the run's own key", got)
	}
	for _, name := range podCredentialEnvVars {
		if name == "ANTHROPIC_API_KEY" {
			continue
		}
		if v, present := env[name]; !present || v != "" {
			t.Errorf("%s crossed as %q (present=%v), want the explicit empty — the pod's credential is not a stamped run's, and an absent name lets the container's own value stand in", name, v, present)
		}
	}
	for name, v := range env {
		if strings.HasPrefix(v, "pod-") {
			t.Errorf("%s carries a credential of the pod into a stamped run's container", name)
		}
	}
	for name, want := range map[string]string{
		"ANTHROPIC_BASE_URL":                "https://gateway.example/anthropic",
		"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1",
		"ITERION_LLM_USER_AGENT":            "operator-agent",
	} {
		if got := env[name]; got != want {
			t.Errorf("%s = %q under the stamp, want the operator's %q — it authenticates nothing", name, got, want)
		}
	}
}

// A stamped node no credential of its run funds is refused on the launcher,
// ErrAmbientCredentialRefused, before any runner starts — the in-process
// registry's answer for the same walk, where the container, which never
// learns the stamp, would resolve on nothing. One row per shape, each beside
// a funded twin or the unstamped bench.
func TestAStampedNodeItsRunDoesNotFundIsRefusedBeforeTheRunner(t *testing.T) {
	pod := stampBench(t)
	codexForfait := map[string]string{string(secrets.OAuthKindCodex): t.TempDir()}

	// The bench: unstamped, the pod funds the node.
	twin, _, err := forwardableProviderEnv(context.Background(), "anthropic/claude-sonnet-4", "")
	if err != nil {
		t.Fatalf("inert bench: the unstamped crossing: %v", err)
	}
	if got := twin["ANTHROPIC_API_KEY"]; got != pod("ANTHROPIC_API_KEY") {
		t.Fatalf("inert bench: the unstamped crossing carries ANTHROPIC_API_KEY=%q, want the pod's", got)
	}

	for _, tc := range []struct {
		name    string
		spec    string
		creds   secrets.Credentials
		refused bool
		// carries is the channel a funded row hands the container.
		carries string
	}{
		{name: "a claude node, nothing held", spec: "anthropic/claude-sonnet-4", refused: true},
		{name: "a claude node beside the run's ChatGPT forfait, which funds an openai node alone",
			spec: "anthropic/claude-sonnet-4", creds: secrets.Credentials{OAuthCredentialFiles: codexForfait}, refused: true},
		{name: "a GLM node holding an Anthropic key, which crosses for an anthropic node alone",
			spec: "anthropic/glm-5.3", creds: secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"}}, refused: true},
		{name: "a gateway node holding no pair", spec: "openai_compatible/m", refused: true},
		{name: "an openai node on the run's ChatGPT forfait",
			spec: "openai/gpt-5.5", creds: secrets.Credentials{OAuthCredentialFiles: codexForfait}, carries: "CODEX_HOME"},
		{name: "a GLM node holding a z.ai key",
			spec: "anthropic/glm-5.3", creds: secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}}, carries: "ZAI_API_KEY"},
		{name: "a gateway node holding its pair",
			spec: "openai_compatible/m", creds: secrets.Credentials{
				APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "gw-run"},
				APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "https://run-gw.example"},
			}, carries: compatgw.APIKeyEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := tc.creds
			creds.AmbientRefused = true
			ctx := secrets.WithCredentials(context.Background(), creds)
			env, _, err := forwardableProviderEnv(ctx, tc.spec, "")
			var refusal *ErrAmbientCredentialRefused
			if !tc.refused {
				if err != nil {
					t.Fatalf("a stamped node its run funds was refused: %v", err)
				}
				if v := env[tc.carries]; v == "" || strings.HasPrefix(v, "pod-") {
					t.Errorf("%s = %q, want the run's own channel", tc.carries, v)
				}
				return
			}
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want the typed ambient refusal — the container would resolve this node on nothing of the run's", err)
			}
			if env != nil {
				t.Errorf("the refused crossing still returned an env of %d names", len(env))
			}
			spend, err := sandboxedClawSpend(ctx, tc.spec, "", credroute.SandboxHomeState{})
			var r *credroute.Refusal
			if !errors.As(err, &r) || r.Reason != credroute.ReasonAmbientStamp {
				t.Errorf("the claw arm answered %+v, %v for the refused node, want its ambient-stamp refusal", spend, err)
			}
		})
	}
}

// A z.ai key is no key of a claude node: the crossing hands it to a GLM node
// alone, and the env factory refuses its synthesis for a claude id. So a run
// holding one beside its Claude forfait crosses that forfait to a sandboxed
// `anthropic/claude-*` node — the instrument the in-process twin spends —
// stamped or not: the stamped crossing does not refuse a node its run funds,
// and neither crossing hands the container an anthropic-wire credential of the
// pod in the forfait's stead.
func TestAZAIKeyKeepsNoClaudeNodeFromItsRunsForfait(t *testing.T) {
	stampBench(t)
	// The forfait's own wire, and no refusal of it: the bench's gateway and
	// switch would keep any forfait home whatever the run holds.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	t.Cleanup(SetOAuthDirLookup(RunOAuthDirLookup))
	const spec = "anthropic/claude-sonnet-4"
	forfait := map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)}

	for _, row := range []struct {
		name    string
		stamped bool
	}{{"unstamped", false}, {"stamped", true}} {
		t.Run(row.name, func(t *testing.T) {
			ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
				AmbientRefused:       row.stamped,
				APIKeys:              map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"},
				OAuthCredentialFiles: forfait,
			})
			// The bench: in process, the registry serves the node on the run's
			// forfait — the run funds it.
			client, err := NewRegistry().ResolveWithContext(ctx, spec)
			if err != nil {
				t.Fatalf("bench: the in-process twin refused the node: %v", err)
			}
			if tok := clientBearerForTest(client); tok != "sk-ant-oat-run" {
				t.Fatalf("bench: the in-process twin carries bearer %q, want the run's forfait", tok)
			}

			env, _, err := forwardableProviderEnv(ctx, spec, "")
			if err != nil {
				t.Fatalf("the crossing refused a node its run's forfait funds: %v", err)
			}
			if got := env["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
				t.Errorf("CLAUDE_CONFIG_DIR = %q, want the run's forfait %q — the z.ai key kept it home", got, secrets.ClaudeCodeSandboxConfigDir)
			}
			for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY"} {
				if v := env[name]; v != "" {
					t.Errorf("%s = %q crosses beside the forfait, which it outranks in the container", name, v)
				}
			}
			if !row.stamped {
				return
			}
			// Stamped, every credential of the pod crossed empty: the claw arm
			// lands the container's walk on the run's forfait.
			if spend, err := sandboxedClawSpend(ctx, spec, "", credroute.SandboxHomeState{}); err != nil || spend.Channel != credroute.ChannelRunForfait || spend.Slot != string(secrets.OAuthKindClaudeCode) {
				t.Errorf("the claw arm = %+v, %v, want the run's forfait", spend, err)
			}
		})
	}
}

// The forfait arm keeps the crossing's explicit-empty promise: each name that
// would outrank the run's forfait in the container crosses present and empty,
// never absent — an absent name is one the container's own env (the sandbox
// spec's, a declared secret's) or the runner's workspace .env fills, and that
// value is spent before the forfait. Stamped, where the pod's credentials
// already crossed empty, and unstamped, where the arm alone keeps them out.
func TestTheForfaitCrossingLeavesNoShadowNameAbsent(t *testing.T) {
	stampBench(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	forfait := map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)}

	for _, row := range []struct {
		name    string
		stamped bool
	}{{"unstamped", false}, {"stamped", true}} {
		t.Run(row.name, func(t *testing.T) {
			ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
				AmbientRefused:       row.stamped,
				OAuthCredentialFiles: forfait,
			})
			env, _, err := forwardableProviderEnv(ctx, "anthropic/claude-sonnet-4", "")
			if err != nil {
				t.Fatalf("forwardableProviderEnv: %v", err)
			}
			if got := env["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
				t.Fatalf("bench: CLAUDE_CONFIG_DIR = %q, want the run's forfait — the arm under test did not run", got)
			}
			for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY"} {
				if v, present := env[name]; !present || v != "" {
					t.Errorf("%s crossed as %q (present=%v), want the explicit empty — absent, a value the container carries under that name outranks the forfait", name, v, present)
				}
			}
		})
	}
}

// A `provider: "anthropic"` claude node goes to Anthropic direct whatever the
// pod's ANTHROPIC_BASE_URL says — the hint's whole point on a pod redirected
// to z.ai or a gateway (#1718) — so its run's Claude forfait is judged on the
// direct wire, by the crossing as by the in-process twin, and the container,
// which re-stamps the hint, spends it there. Stamped, the crossing does not
// refuse a node its run funds; unstamped, it hands the container no
// credential of the pod in the forfait's stead. The admission probe reads the
// same hint the dispatch does.
func TestAHintedClaudeNodeCrossesItsRunsForfaitPastARedirectedWire(t *testing.T) {
	stampBench(t)
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	t.Cleanup(SetOAuthDirLookup(RunOAuthDirLookup))
	const spec, hint = "anthropic/claude-sonnet-4", "anthropic"
	forfaitDir := forfaitDirForTest(t)
	forfait := map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDir}

	for _, wire := range []string{"https://api.z.ai/api/anthropic", "https://gateway.example/anthropic"} {
		for _, stamped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s stamped=%v", wire, stamped), func(t *testing.T) {
				t.Setenv("ANTHROPIC_BASE_URL", wire)
				ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
					AmbientRefused:       stamped,
					OAuthCredentialFiles: forfait,
				})
				// The bench: in process the hinted node is served on the run's
				// forfait, and the hint alone decides — the hint-less crossing
				// on the same wire leaves the forfait home.
				client, err := NewRegistry().ResolveWithContext(WithProviderHint(ctx, hint), spec)
				if err != nil {
					t.Fatalf("bench: the in-process twin refused the hinted node: %v", err)
				}
				if tok := clientBearerForTest(client); tok != "sk-ant-oat-run" {
					t.Fatalf("bench: the in-process twin carries bearer %q, want the run's forfait", tok)
				}
				if hintless, _, _ := forwardableProviderEnv(ctx, spec, ""); hintless["CLAUDE_CONFIG_DIR"] != "" {
					t.Fatalf("inert bench: the hint-less crossing on %s carries the forfait too — the hint would decide nothing", wire)
				}

				env, _, err := forwardableProviderEnv(ctx, spec, hint)
				if err != nil {
					t.Fatalf("the crossing refused a node its run's forfait funds: %v", err)
				}
				if got := env["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
					t.Errorf("CLAUDE_CONFIG_DIR = %q, want the run's forfait %q — judged on the wire the hint skips", got, secrets.ClaudeCodeSandboxConfigDir)
				}
				for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY"} {
					if v, present := env[name]; !present || v != "" {
						t.Errorf("%s crossed as %q (present=%v), want the explicit empty beside the forfait", name, v, present)
					}
				}
				// Stamped, every credential of the pod crossed empty: the claw
				// arm lands the hinted leg's container walk on the run's forfait.
				if stamped {
					if spend, err := sandboxedClawSpend(ctx, spec, hint, credroute.SandboxHomeState{}); err != nil || spend.Channel != credroute.ChannelRunForfait {
						t.Errorf("the claw arm on the %q leg = %+v, %v, want the run's forfait", hint, spend, err)
					}
				}

				// The container's own resolution over the crossed env: no run
				// credentials on its context, the hint re-stamped, the forfait
				// at the dir the runtime seeds.
				for name, v := range env {
					t.Setenv(name, v)
				}
				t.Setenv("CLAUDE_CONFIG_DIR", forfaitDir)
				inContainer, err := NewRegistry().ResolveWithContext(WithProviderHint(context.Background(), hint), spec)
				if err != nil {
					t.Fatalf("the container refused the hinted node over the crossed env: %v", err)
				}
				if tok := clientBearerForTest(inContainer); tok != "sk-ant-oat-run" {
					t.Errorf("the container carries bearer %q, want the run's forfait its in-process twin spends", tok)
				}
			})
		}
	}
}

// Under ITERION_FORBID_SUBSCRIPTION_OAUTH the in-process twin passes the run's
// forfait by for an ambient Anthropic credential of the pod, and refuses it
// only where it is the walk's sole candidate. The crossing reads the same
// walk: beside the pod's key it hands the container that key unstamped (the
// account the in-process twin spends) and refuses stamped (the in-process
// twin's answer too); with no ambient credential it crosses the forfait, so
// the container answers the switch's refusal by name.
func TestTheForbidSwitchCrossesWhatTheInProcessTwinSpends(t *testing.T) {
	pod := stampBench(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	t.Cleanup(SetOAuthDirLookup(RunOAuthDirLookup))
	const spec = "anthropic/claude-sonnet-4"
	forfait := map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)}
	forfaitCtx := func(stamped bool) context.Context {
		return secrets.WithCredentials(context.Background(), secrets.Credentials{
			AmbientRefused:       stamped,
			OAuthCredentialFiles: forfait,
		})
	}

	t.Run("beside the pod's key, unstamped", func(t *testing.T) {
		ctx := forfaitCtx(false)
		client, err := NewRegistry().ResolveWithContext(ctx, spec)
		if cc, ok := client.(*api.Client); err != nil || !ok || cc.APIKey != pod("ANTHROPIC_API_KEY") {
			t.Fatalf("bench: the in-process twin = %T err=%v, want a client on the pod's key", client, err)
		}
		env, _, err := forwardableProviderEnv(ctx, spec, "")
		if err != nil {
			t.Fatalf("forwardableProviderEnv: %v", err)
		}
		if got := env["CLAUDE_CONFIG_DIR"]; got != "" {
			t.Errorf("CLAUDE_CONFIG_DIR = %q crosses under the switch beside the pod's key — the container refuses a node its in-process twin serves", got)
		}
		if got := env["ANTHROPIC_API_KEY"]; got != pod("ANTHROPIC_API_KEY") {
			t.Errorf("ANTHROPIC_API_KEY = %q, want the pod's key the in-process twin spends", got)
		}
	})
	t.Run("beside the pod's key, stamped", func(t *testing.T) {
		ctx := forfaitCtx(true)
		var refusal *ErrAmbientCredentialRefused
		if _, err := NewRegistry().ResolveWithContext(ctx, spec); !errors.As(err, &refusal) {
			t.Fatalf("bench: the in-process twin answered %v, want the typed ambient refusal", err)
		}
		if _, _, err := forwardableProviderEnv(ctx, spec, ""); !errors.As(err, &refusal) {
			t.Errorf("the crossing answered %v, want the in-process twin's typed ambient refusal — not a forfait the switch refuses in the container", err)
		}
	})
	t.Run("the forfait the sole candidate", func(t *testing.T) {
		for _, name := range podCredentialEnvVars {
			t.Setenv(name, "")
		}
		ctx := forfaitCtx(false)
		if _, err := NewRegistry().ResolveWithContext(ctx, spec); !errors.Is(err, secrets.ErrSubscriptionOAuthForbidden) {
			t.Fatalf("bench: the in-process twin answered %v, want the switch's refusal", err)
		}
		env, _, err := forwardableProviderEnv(ctx, spec, "")
		if err != nil {
			t.Fatalf("forwardableProviderEnv: %v", err)
		}
		if got := env["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
			t.Errorf("CLAUDE_CONFIG_DIR = %q, want the run's forfait — the container's refusal of it is the in-process twin's answer", got)
		}
		if got := env["ITERION_FORBID_SUBSCRIPTION_OAUTH"]; got != "1" {
			t.Errorf("ITERION_FORBID_SUBSCRIPTION_OAUTH = %q in the container, want the operator's refusal", got)
		}
	})
}
