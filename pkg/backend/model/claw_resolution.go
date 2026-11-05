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
	// the guard runs here, launcher side. It is the node dispatch's guard: a
	// supervisor's evaluator resolves through the registry's walk alone,
	// which judges the switch on what it reaches.
	if call.Role == credroute.RoleNode || call.Role == "" {
		if err := clawSubscriptionGuard(ctx, spec); err != nil {
			return clawResolution{}, clawRefusal(call, err)
		}
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

// inProcessClaw is the claw arm's answer for a call this process makes on the
// host: the client the arm's walk built and the Spend that call's usage
// events carry (ADR-161 decisions 3 and 8), never a second walk. A walk that
// reaches no credential still hands back the client it built — the
// provider's refusal is the call's answer, as it is for every in-process
// call — with the zero Spend; any other refusal is the call's error.
func inProcessClaw(ctx context.Context, reg *Registry, call credroute.Call) (api.APIClient, credroute.Spend, error) {
	call.Sandbox = credroute.Sandbox{}
	res, err := resolveClawCall(ctx, reg, call)
	var refusal *credroute.Refusal
	if err != nil && !(errors.As(err, &refusal) && refusal.Reason == credroute.ReasonNoCredential && res.client != nil) {
		return nil, credroute.Spend{}, err
	}
	return res.client, res.spend, nil
}

// resolveBeside is the claw arm's resolution of spec for a call a node's
// execution makes beside its dispatch — a subagent the node spawns, a human
// node's LLM half, the review companion, a recovery rung: in process, under
// the hint ctx carries (the dispatching node's, which Execute stamps), the
// walk ResolveWithContext takes, and the Spend the call's events carry. Its
// role is auxiliary: Execute's subscription switch guard judges the node's
// own dispatch, and the walk judges the switch on what it reaches.
func (r *Registry) resolveBeside(ctx context.Context, spec string) (api.APIClient, credroute.Spend, error) {
	return inProcessClaw(ctx, r, credroute.Call{
		Backend: delegate.BackendClaw, Spec: spec,
		Leg:  credroute.Leg{Provider: providerHintFromContext(ctx)},
		Role: credroute.RoleAuxiliary,
	})
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
// compatgw's names, refused where the gateway factory refuses the pair
// (gatewayEnvConfig). Every other provider spends what its own env factory
// reads (clawEnvFactories): the key under that factory's name, or the pod's
// identity chain where the factory serves without one — refused where the
// factory refuses what it is handed (clawEnvFactory.refuse, the predicate
// the factory calls: moonshot's facade refuses a non-kimi id here as in
// process). A spec the container's registry cannot resolve — unreadable, or
// a provider it registers no factory for — is refused, as the container
// refuses it.
func containerSpend(ctx context.Context, env map[string]string, facts crossingFacts, spec, hint string, home credroute.SandboxHomeState) (credroute.Spend, error) {
	creds, _ := secrets.CredentialsFromContext(ctx)
	lookup := func(name string) string { return env[name] }
	inside := credroute.WithEnv(context.Background(), env)
	seeded := ""
	if env["CLAUDE_CONFIG_DIR"] == secrets.ClaudeCodeSandboxConfigDir {
		seeded = secrets.AnthropicForfaitAccessToken(creds.OAuthDir(string(secrets.OAuthKindClaudeCode)))
	}
	inside = context.WithValue(inside, containerDiskKey{}, containerDisk{claudeForfaitToken: seeded})
	// crossed is the Spend of the credential the container's walk takes from
	// the variable name, sent where the walk sends it: the crossing's record
	// of name (crossingRecord.spendOf, the rule the container's own walk
	// names its calls by).
	crossed := func(name string, walk credroute.Spend) (credroute.Spend, error) {
		if name == "" || env[name] == "" {
			return credroute.Spend{}, &errNoClawCredential{Spec: spec}
		}
		spend, ok := crossingRecord(facts.funds).spendOf(name, walk)
		if !ok {
			return credroute.Spend{}, fmt.Errorf("model: the crossing set %s without naming where its credential comes from", name)
		}
		return spend, nil
	}
	at := func(baseURL string) credroute.Spend { return credroute.Spend{BaseURL: baseURL} }

	provider, modelID, err := ParseModelSpec(spec)
	if err != nil {
		return credroute.Spend{}, err
	}
	switch provider {
	case "anthropic":
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
		// The pod's account as the container's client carries it: the walk
		// read the crossed values, the pod's own.
		return crossed(w.envName(), w.spend())
	case "openai":
		apiKey := env["OPENAI_API_KEY"]
		if openAIOAuthAllowed(inside) && (apiKey == "" || env["ITERION_OPENAI_USE_OAUTH"] == "1") {
			if env["CODEX_HOME"] == secrets.CodexSandboxConfigDir {
				return crossed("CODEX_HOME", credroute.Spend{})
			}
			if spend, ok := hostCodexForfaitSpend(ctx, spec, home); ok {
				return spend, nil
			}
		}
		return crossed("OPENAI_API_KEY", at(env["OPENAI_BASE_URL"]))
	case modelroute.OpenAICompatible:
		// The gateway env factory's own refusal of the pair that crossed.
		cfg, err := gatewayEnvConfig(compatgw.FromEnv(lookup))
		if err != nil {
			return credroute.Spend{}, err
		}
		return crossed(compatgw.APIKeyEnv, at(cfg.BaseURL))
	}
	factory, ok := clawEnvFactories[provider]
	if !ok {
		return credroute.Spend{}, fmt.Errorf("model: unknown provider %q (spec: %q): the in-container registry builds no client for it", provider, spec)
	}
	keyed := factory.key != "" && env[factory.key] != ""
	if !keyed && !factory.identityChain {
		return credroute.Spend{}, &errNoClawCredential{Spec: spec}
	}
	// The factory's own refusal of what it is handed, the predicate it
	// calls itself: the verdict cannot depend on where the call is placed.
	if factory.refuse != nil {
		if err := factory.refuse(modelID, env[factory.baseURL]); err != nil {
			return credroute.Spend{}, err
		}
	}
	if keyed {
		return crossed(factory.key, at(env[factory.baseURL]))
	}
	// The factory's SDK serves a keyless client from the pod's own identity
	// chain (instance metadata, workload identity, ADC).
	return credroute.Spend{Channel: credroute.ChannelPodEnv, BaseURL: env[factory.baseURL]}, nil
}

// clawEnvFactory is what one env factory of claw's registry (registerDefaults)
// reads its credential from, for the providers containerSpend does not walk
// branch by branch.
type clawEnvFactory struct {
	// key is the variable the factory reads the account key from; "" for a
	// factory that reads none.
	key string
	// baseURL is the variable naming the destination that key is sent to;
	// "" for a factory whose destination no variable redirects.
	baseURL string
	// identityChain: with no key, the factory's SDK serves from the pod's
	// own identity chain rather than building a credential-less client.
	identityChain bool
	// refuse is the factory's own refusal of modelID sent to the raw value of
	// its baseURL variable, the predicate registerDefaults' factory calls
	// itself — nil for a factory iterion's code refuses nothing in. A cloud
	// SDK's own configuration checks (the endpoint, the project, the AWS
	// chain) are claw's internal providers' and read the process env: no
	// reader can ask them ahead of the call.
	refuse func(modelID, baseURL string) error
}

// clawEnvFactories names, per provider of registerDefaults other than
// anthropic, openai and the gateway (containerSpend's own branches), the
// credential its env factory reads in the container. The crossing writes a
// run's key for the node under that same name (byokEnvVar) beside whatever
// the pod forwards under the others, so the name the factory reads decides
// which of them the call spends. TestEveryDefaultProviderNamesItsContainerCredential
// pins every registered factory to an entry here.
var clawEnvFactories = map[string]clawEnvFactory{
	"xai":      {key: "XAI_API_KEY", baseURL: "XAI_BASE_URL"},
	"moonshot": {key: "MOONSHOT_API_KEY", baseURL: "MOONSHOT_BASE_URL", refuse: moonshotModelRefusal},
	// azidentity's DefaultAzureCredential serves without the key.
	"foundry": {key: "AZURE_OPENAI_API_KEY", baseURL: "AZURE_OPENAI_ENDPOINT", identityChain: true},
	// The AWS SDK's chain: the key pair when it crossed, the pod's role
	// otherwise.
	"bedrock": {key: "AWS_ACCESS_KEY_ID", identityChain: true},
	// Google's ADC: no variable of the crossing carries it.
	"vertex": {identityChain: true},
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
// here. stampedCrossingFunds asks whether any of them is set; which one a
// call spends is containerSpend's walk, never the list's order.
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
