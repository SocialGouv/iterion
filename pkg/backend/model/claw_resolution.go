package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The claw arm (ADR-161, U1): claw's precedence — the switch guard, the
// registry's walk in process, the crossing and the container's walk over what
// crossed — answering credroute.Resolve for every claw leg. The dispatch
// (ClawBackend.Execute) and the launch's admission read the same function,
// resolveClawCall: the dispatch builds its client, or hands the container its
// env, from the resolution it returns, and never walks again.
//
// Registered as a literal calling clawArm, so the arm's env reads stay
// attributed to their call sites (claw_sandbox_env_class_test.go).
func init() {
	credroute.Register(delegate.BackendClaw, func(ctx context.Context, call credroute.Call) (credroute.Spend, error) {
		return clawArm(ctx, call)
	})
}

// clawArm is the registered arm, over the engine's own registry.
func clawArm(ctx context.Context, call credroute.Call) (credroute.Spend, error) {
	res, err := resolveClawCall(ctx, NewRegistry(), call)
	return res.spend, err
}

// clawResolution is what resolveClawCall settles for one leg: the spend and,
// for the dispatch, what it is spent through.
type clawResolution struct {
	spend credroute.Spend
	// client is the client an in-process call sends with.
	client api.APIClient
	// env is what the crossing hands the container for a sandboxed call, and
	// facts what it refused and what each credential it carries is.
	env   map[string]string
	facts crossingFacts
}

// errNoClawCredential is the cause of a no-credential refusal where the walk
// itself raised none: it built, or would have the container build, a client
// carrying no credential at all — the call answers 401 at the provider.
type errNoClawCredential struct {
	Spec string
}

func (e *errNoClawCredential) Error() string {
	return fmt.Sprintf("model: claw's walk for %q reaches no credential — no run credential, no credential of the environment, no login", e.Spec)
}

// resolveClawCall is claw's precedence for one leg, over reg's walk in
// process. The posture is captured once and pinned on ctx: every read of the
// call's environment below answers from it.
//
// A call with no credential still carries the client the walk built
// (res.client) beside its no-credential refusal: an in-process dispatch sends
// it, as before, and the provider's own refusal is the call's answer.
func resolveClawCall(ctx context.Context, reg *Registry, call credroute.Call) (clawResolution, error) {
	ctx = credroute.WithPosture(ctx, credroute.Capture(ctx))
	spec, hint := call.Spec, call.Leg.Provider
	// Execute's subscription switch, before either placement: under
	// ITERION_FORBID_SUBSCRIPTION_OAUTH a node whose run funds its Anthropic
	// id with the Claude forfait alone is refused, whatever the pod's env
	// carries — the in-container runner never sees the run's credentials, so
	// the guard runs here, launcher side.
	if err := clawSubscriptionGuard(ctx, spec); err != nil {
		return clawResolution{}, clawRefusal(call, err)
	}
	if !call.Sandbox.Contained {
		client, spend, err := reg.resolveSpend(WithProviderHint(ctx, hint), spec)
		if err != nil {
			return clawResolution{}, clawRefusal(call, err)
		}
		res := clawResolution{spend: spend, client: client}
		if spend.Channel == "" {
			return res, clawRefusal(call, &errNoClawCredential{Spec: spec})
		}
		return res, nil
	}
	env, facts, err := forwardableProviderEnv(ctx, spec, hint)
	if err != nil {
		return clawResolution{}, clawRefusal(call, err)
	}
	res := clawResolution{env: env, facts: facts}
	spend, err := containerSpend(ctx, env, facts, spec, hint, call.Sandbox.Home)
	if err != nil {
		return res, clawRefusal(call, err)
	}
	res.spend = spend
	return res, nil
}

// clawRefusal types err, a refusal of the walk or the crossing, as the
// resolver's Refusal, unwrapping to err: every errors.As a site matches today
// keeps matching.
func clawRefusal(call credroute.Call, err error) *credroute.Refusal {
	provider, _, _ := ParseModelSpec(call.Spec)
	r := &credroute.Refusal{Backend: call.Backend, Provider: provider, Spec: call.Spec, Leg: call.Leg, Cause: err}
	var ambient *ErrAmbientCredentialRefused
	var hintUnfunded *ErrProviderHintUnfunded
	var glmUnfunded *ErrGLMHintUnfunded
	switch {
	case errors.As(err, &ambient):
		r.Reason = credroute.ReasonAmbientStamp
	case errors.Is(err, secrets.ErrSubscriptionOAuthForbidden):
		r.Reason = credroute.ReasonSubscriptionSwitch
	case errors.As(err, &hintUnfunded), errors.As(err, &glmUnfunded):
		r.Reason = credroute.ReasonHintUnfunded
	case errors.Is(err, ErrProviderOutsideAllowlist), errors.Is(err, ErrRouteUnreadableUnderAllowlist):
		r.Reason = credroute.ReasonOutsideAllowlist
	case errors.Is(err, credroute.ErrPoolContentRefused):
		r.Reason = credroute.ReasonContentBoundary
	default:
		// The walk built nothing that authenticates: the synthesis's typed
		// refusal of an id z.ai does not serve, a misconfigured gateway
		// env, a lapsed forfait, an unreadable spec — or no credential.
		r.Reason = credroute.ReasonNoCredential
	}
	return r
}

// containerSpend names what the in-container runner's own resolution lands on
// over env, the env the crossing composed: the runner rebuilds its registry
// from that env alone — ctx, and so the run's credentials, never cross —
// under the node's hint, which it re-stamps, so the walk below reads env and
// nothing of the host's. Each credential the walk takes names the channel the
// crossing filled it from (facts.funds). home is what the sandbox binds of
// the operator's home.
//
// claw's anthropic wire is walked as the container walks it (the direct path
// under the hint, the env factory otherwise), and so is the openai wire's
// ChatGPT forfait and key order; a gateway route spends what crossed under
// compatgw's names. Any other spec reads the first credential the crossing
// hands the container: its factory reads the pod's own credential chain.
func containerSpend(ctx context.Context, env map[string]string, facts crossingFacts, spec, hint string, home credroute.SandboxHomeState) (credroute.Spend, error) {
	creds, _ := secrets.CredentialsFromContext(ctx)
	inside := credroute.WithEnv(context.Background(), env)
	seeded := ""
	if env["CLAUDE_CONFIG_DIR"] == secrets.ClaudeCodeSandboxConfigDir {
		seeded = secrets.AnthropicForfaitAccessToken(creds.OAuthDir(string(secrets.OAuthKindClaudeCode)))
	}
	inside = context.WithValue(inside, containerDiskKey{}, containerDisk{claudeForfaitToken: seeded})
	crossed := func(name, baseURL string) (credroute.Spend, error) {
		if name == "" || env[name] == "" {
			return credroute.Spend{}, &errNoClawCredential{Spec: spec}
		}
		spend, ok := facts.funds[name]
		if !ok {
			return credroute.Spend{}, fmt.Errorf("model: the crossing set %s without naming where its credential comes from", name)
		}
		if baseURL != "" {
			spend.BaseURL = baseURL
		}
		return spend, nil
	}

	provider, modelID, err := ParseModelSpec(spec)
	switch {
	case err == nil && provider == "anthropic":
		direct := HonoursProviderHintValue(delegate.BackendClaw, hint)
		var w anthropicEnvWalk
		switch {
		case direct && !modelServedByZAI(modelID):
			w, err = anthropicDirectEnvCredentials(inside, modelID)
		case direct && !glmKeyReachableUnderHint(inside):
			return credroute.Spend{}, &ErrGLMHintUnfunded{Model: modelID}
		default:
			w, err = anthropicEnvCredentials(inside, modelID)
		}
		if err != nil {
			return credroute.Spend{}, err
		}
		return crossed(w.envName(), w.baseURL)
	case err == nil && provider == "openai":
		apiKey := env["OPENAI_API_KEY"]
		if openAIOAuthAllowed(inside) && (apiKey == "" || env["ITERION_OPENAI_USE_OAUTH"] == "1") {
			if env["CODEX_HOME"] == secrets.CodexSandboxConfigDir {
				return crossed("CODEX_HOME", "")
			}
			if spend, ok := hostCodexForfaitSpend(ctx, spec, home); ok {
				return spend, nil
			}
		}
		return crossed("OPENAI_API_KEY", env["OPENAI_BASE_URL"])
	case err == nil && provider == modelroute.OpenAICompatible:
		return crossed(compatgw.APIKeyEnv, env[compatgw.BaseURLEnv])
	}
	for _, key := range clawSandboxCredentialEnvKeys {
		if env[key] != "" {
			return crossed(key, "")
		}
	}
	if spend, ok := hostCodexForfaitSpend(ctx, spec, home); ok {
		return spend, nil
	}
	return credroute.Spend{}, &errNoClawCredential{Spec: spec}
}

// hostCodexForfaitSpend names the host's ChatGPT login when it reaches a
// sandboxed node: the copy deliverHostCodexForfait writes into the container
// at dispatch (hostCodexForfait — its switch and planted-home refusals
// included), else the operator's ~/.codex the host_state mount carries.
func hostCodexForfaitSpend(ctx context.Context, spec string, home credroute.SandboxHomeState) (credroute.Spend, bool) {
	if payload, _ := hostCodexForfait(ctx, spec); payload != nil {
		return credroute.Spend{Channel: credroute.ChannelHostLogin}, true
	}
	if home.Mounted && home.HomeDir != "" {
		if _, err := os.Stat(filepath.Join(home.HomeDir, ".codex", "auth.json")); err == nil {
			return credroute.Spend{Channel: credroute.ChannelHostLogin}, true
		}
	}
	return credroute.Spend{}, false
}

// clawSandboxCredentialEnvKeys are the entries of the env
// forwardableProviderEnv hands a sandboxed claw node that authenticate it:
// the provider keys, the bearer, the gateway key, and the two secret-FILE
// channels (CODEX_HOME / CLAUDE_CONFIG_DIR point at a forfait the runtime
// seeded into the container). Endpoint and routing names
// (providerRoutingEnvVars) never authenticate by themselves and are not
// here.
var clawSandboxCredentialEnvKeys = []string{
	"OPENAI_API_KEY",
	"AZURE_OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ZAI_API_KEY",
	"MOONSHOT_API_KEY",
	"XAI_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GROQ_API_KEY",
	"DEEPSEEK_API_KEY",
	"MISTRAL_API_KEY",
	"AWS_ACCESS_KEY_ID",
	compatgw.APIKeyEnv,
	"CODEX_HOME",
	"CLAUDE_CONFIG_DIR",
}
