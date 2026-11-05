package credparity

import (
	"github.com/SocialGouv/iterion/pkg/secrets"
)

const (
	claudeSpec = "anthropic/claude-sonnet-4"
	glmSpec    = "anthropic/glm-5.3"
	gatewayURL = "https://llm-gw.example.com"
	zaiWireURL = "https://api.z.ai/api/anthropic"
)

// claw is a claw node on the Claude spec under the raw provider field, its
// dispatch chain legs.
func claw(provider string, legs ...Leg) Node {
	if len(legs) == 0 {
		legs = []Leg{{}}
	}
	return Node{ID: "impl", Backend: "claw", Model: claudeSpec, Provider: provider, Legs: legs}
}

// onBackend is a node on backend sending model, its chain one leg that
// defers to the backend's own precedence.
func onBackend(backend, model string) Node {
	return Node{ID: "impl", Backend: backend, Model: model, Legs: []Leg{{}}}
}

// sides gives the row its two placements: on the host (a trusted run with
// `sandbox: none`) and in the sandbox (the fork lane). host and sandboxed
// edit the copy each side gets.
func sides(r Row, host, sandboxed func(*Row)) []Row {
	h, s := r, r
	h.Name += " (host)"
	h.Trust, h.Sandboxed = Trusted, false
	s.Name += " (sandboxed)"
	s.Trust, s.Sandboxed = Fork, true
	if host != nil {
		host(&h)
	}
	if sandboxed != nil {
		sandboxed(&s)
	}
	return []Row{h, s}
}

// stampings gives the row unstamped and stamped twins.
func stampings(r Row, unstamped, stamped func(*Row)) []Row {
	u, s := r, r
	u.Name += ", unstamped"
	s.Name += ", stamped"
	s.Run.Stamped = true
	if unstamped != nil {
		unstamped(&u)
	}
	if stamped != nil {
		stamped(&s)
	}
	return []Row{u, s}
}

// Rows is the parity table.
func Rows() []Row {
	var rows []Row
	add := func(rs ...Row) { rows = append(rows, rs...) }

	// A z.ai key never funds a Claude id: the run's forfait does, in process
	// and across the sandbox, past the pod's key.
	for _, r := range stampings(Row{
		Name:    "a z.ai key beside the run's forfait",
		Origin:  "bench dd5511f08 TestAZAIKeyKeepsNoClaudeNodeFromItsRunsForfait (t49 r2, t50 r1)",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Forfait: true},
		Node:    claw(""),
		Want:    Spent(0, RunForfait, string(secrets.OAuthKindClaudeCode), ForfaitToken),
	}, nil, nil) {
		add(sides(r, nil, nil)...)
	}

	// A redirected wire keeps the forfait home: the pod's key spends, and
	// under the stamp nothing does.
	for _, r := range stampings(Row{
		Name:    "a gateway wire beside the run's forfait",
		Origin:  "bench 6e2389644 TestAClawClaudeRouteMetersWhatClawsWalkSpendsOffTheForfaitsWire (t50 r1 #2)",
		Posture: map[string]string{"ANTHROPIC_BASE_URL": gatewayURL, "ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Forfait: true},
		Node:    claw(""),
		Want:    Spent(0, PodEnv, "", PodAnthropic),
	}, nil, func(r *Row) { r.Want = Refuses(AmbientStamp) }) {
		add(sides(r, nil, nil)...)
	}

	// `provider: "anthropic"` goes to Anthropic direct past a z.ai wire: the
	// run's forfait is judged there, and spends. The ledger booked the spend
	// on nobody — its route carried no hint, and the hint-less walk keeps the
	// forfait home (the bench's t50 r1 fix, deferred) — until U2's claw
	// events carried the arm's slot.
	for _, r := range stampings(Row{
		Name:    "a hinted Claude node past a z.ai wire",
		Origin:  "bench 6e2389644 TestAHintedClaudeNodeCrossesItsRunsForfaitPastARedirectedWire (t50 r1 #1)",
		Posture: map[string]string{"ANTHROPIC_BASE_URL": zaiWireURL, "ZAI_API_KEY": PodZAI},
		Run:     Run{Forfait: true},
		Node:    claw("anthropic", Leg{Provider: "anthropic"}),
		Want:    Spent(0, RunForfait, string(secrets.OAuthKindClaudeCode), ForfaitToken),
	}, nil, nil) {
		add(sides(r, nil, nil)...)
	}

	// SEEN RED (t50 r2 #1): ClawBackend.Execute's switch guard refuses the
	// node before either arm, the pod's key untouched; the meter keyed the
	// pod's account, and the probe, which never met the guard, admitted — U1
	// moved the guard into the claw arm the admission asks, U2 the meter
	// onto the arm's answer.
	for _, r := range stampings(Row{
		Name:    "the subscription switch beside the pod's key",
		Origin:  "t50 r2 #1 (open MEDIUM); bench 6e2389644 TestTheForbidSwitchCrossesWhatTheInProcessTwinSpends",
		Posture: map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Forfait: true},
		Node:    claw(""),
		Want:    Refuses(SubscriptionSwitch),
	}, nil, nil) {
		add(sides(r, nil, nil)...)
	}

	// SEEN RED (t50 r2 #2): the probe split the hint on its first comma, raw;
	// dispatch expands and parses the chain, and crosses the run's forfait —
	// U1's admission reads model.Legs, the dispatch's own chain.
	for _, spelling := range []struct{ name, provider string }{
		{"the element form", "anthropic:" + claudeSpec},
		{"a stray leading comma", " ,anthropic"},
		{"an env default", "${CLAW_PROVIDER:-anthropic}"},
	} {
		legs := []Leg{{Provider: "anthropic"}}
		if spelling.name == "the element form" {
			legs = []Leg{{Provider: "anthropic", Model: claudeSpec}}
		}
		add(Row{
			Name:      "a hint in " + spelling.name + " on a z.ai wire (sandboxed)",
			Origin:    "t50 r2 #2 (open LOW)",
			Posture:   map[string]string{"ANTHROPIC_BASE_URL": zaiWireURL},
			Run:       Run{Forfait: true},
			Node:      claw(spelling.provider, legs...),
			Sandboxed: true,
			Trust:     Fork,
			Want:      Spent(0, RunForfait, string(secrets.OAuthKindClaudeCode), ForfaitToken),
		})
	}

	// SEEN RED (the plan row, ADR-161 context): a pool-lent forfait the wire
	// keeps home — the call spends the pod's key, the fine table booked the
	// donor, joining the step's provider word to the publisher's pod-blind
	// plan row; U2 books it on the call's own credential keys.
	add(sides(Row{
		Name:    "a pool-lent forfait beside a gateway wire",
		Origin:  "ADR-161 context (plan ≠ dispatch, t53 r1 #1); #2288 258bc2fbf",
		Posture: map[string]string{"ANTHROPIC_BASE_URL": gatewayURL, "ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Forfait: true, Plan: true},
		Node:    claw(""),
		Want:    Spent(0, PodEnv, "", PodAnthropic),
	}, nil, nil)...)

	// The plan books the forfait it predicted where the call spends it.
	add(sides(Row{
		Name:   "a pool-lent forfait on its own wire",
		Origin: "#2288 c0ff7d09b (the fine table's agreeing shape)",
		Run:    Run{Forfait: true, Plan: true},
		Node:   claw(""),
		Want:   Spent(0, RunForfait, string(secrets.OAuthKindClaudeCode), ForfaitToken),
	}, nil, nil)...)

	// Nothing of the run's nor the pod's: the call carries no credential and
	// dies on the provider's refusal. The meter keyed the scope's
	// no-credential meter (AmbientEnvMeter's bare spend); ADR-161 decision 7
	// reads a refusal as not metered, and U2's meter reads the arm.
	add(sides(Row{
		Name:   "nothing of the run's nor the pod's",
		Origin: "#1768 TestForkRunOnClawWithoutCredentialChannelIsRefused; bench 09ddb1695 TestAmbientEnvMeter_NoEnvCredentialIsTheBareSpend",
		Node:   claw(""),
		Want:   Refuses(NoCredential),
	}, nil, nil)...)

	// SEEN RED (t53 r2 #2): a stamped first leg refused, dispatch falls through
	// to the run's z.ai key; the pre-flight read the first chain element only
	// (AnthropicWireRoutes) and the probe composed the first leg's crossing
	// alone — U1's admission resolves every leg, U2's meter keys the first
	// leg the arm funds.
	add(sides(Row{
		Name:    "a fall-through leg past a stamped first leg",
		Origin:  "t53 r2 #2 (ADR-161 review: a fall-through leg unmetered)",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Stamped: true},
		Node:    claw("anthropic,zai:"+glmSpec, Leg{Provider: "anthropic"}, Leg{Provider: "zai", Model: glmSpec}),
		Want:    Spent(1, RunKey, string(secrets.ProviderZAI), RunZAI),
	}, nil, nil)...)

	// SEEN RED (t53 r2 #4): the allowlist drops the leg the pod funds; the
	// probe, blind to it, admitted on that leg — U1's admission walks the
	// legs model.Legs keeps.
	add(Row{
		Name:      "an allowlist-dropped first leg (sandboxed)",
		Origin:    "t53 r2 #4 (ADR-161 review: an allowlist-dropped leg admitted)",
		Posture:   map[string]string{"ZAI_API_KEY": PodZAI},
		Run:       Run{Allowlist: []string{"anthropic"}},
		Node:      claw("zai:"+glmSpec+",anthropic", Leg{Provider: "anthropic"}),
		Sandboxed: true,
		Trust:     Fork,
		Want:      Refuses(HintUnfunded),
	})

	// SEEN RED (t53 r2 #3): a stamped run on the host passed no admission —
	// U1's one site judges it before the host exit.
	add(Row{
		Name:    "a stamped node its run does not fund (host)",
		Origin:  "t53 r2 #3 (ADR-161 review: an unsandboxed stamped start unjudged)",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Stamped: true},
		Node:    claw(""),
		Trust:   Trusted,
		Want:    Refuses(AmbientStamp),
	})

	// SEEN RED (t53 r3 #2): a subbot child adopting its parent's sandbox
	// passed no admission — U1 judges it in startSandbox's adopt branch.
	add(Row{
		Name:      "a stamped child its run does not fund (adopted)",
		Origin:    "t53 r3 #2 (ADR-161 review: an adopted child unjudged)",
		Posture:   map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:       Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Stamped: true},
		Node:      claw(""),
		Sandboxed: true,
		Trust:     Trusted,
		Adopted:   true,
		Want:      Refuses(AmbientStamp),
	})

	// The stamp refuses the pod's money and keeps the run's own.
	add(Row{
		Name:      "a stamped node its run does not fund (sandboxed)",
		Origin:    "bench 582197432 TestAStampedNodeItsRunDoesNotFundIsRefusedBeforeTheRunner",
		Posture:   map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:       Run{Stamped: true},
		Node:      claw(""),
		Sandboxed: true,
		Trust:     Fork,
		Want:      Refuses(AmbientStamp),
	})
	add(sides(Row{
		Name:    "a stamped run's own key beside the pod's",
		Origin:  "bench 582197432 (a stamped run's own-key meter is pinned)",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderAnthropic: RunAnthropic}, Stamped: true},
		Node:    claw(""),
		Want:    Spent(0, RunKey, string(secrets.ProviderAnthropic), RunAnthropic),
	}, nil, nil)...)

	// SEEN RED (#2022, U0–U2's review): a claw node off the anthropic and
	// openai wires spends the run's own key for its provider, on the host and
	// in the sandbox alike, past the pod's OpenAI key the crossing forwards to
	// every node. The arm named the container's credential as the first
	// forwarded key set — the pod's — where the container's factory reads
	// its provider's own name: the meter, the ledger and the fine table read
	// the pod's money for the run's lent key.
	for _, own := range []struct {
		provider secrets.Provider
		spec     string
		key      string
	}{
		{secrets.ProviderMoonshot, "moonshot/kimi-k2", RunMoonshot},
		{secrets.ProviderXAI, "xai/grok-4", RunXAI},
	} {
		add(sides(Row{
			Name:    "a run's own " + string(own.provider) + " key beside the pod's OpenAI key",
			Origin:  "#2022 U0–U2 review (a sandboxed claw node on moonshot or xai spent the arm's first forwarded key)",
			Posture: map[string]string{"OPENAI_API_KEY": PodOpenAI},
			Run:     Run{Keys: map[secrets.Provider]string{own.provider: own.key}, Plan: true},
			Node:    Node{ID: "impl", Backend: "claw", Model: own.spec, Legs: []Leg{{}}},
			Want:    Spent(0, RunKey, string(own.provider), own.key),
		}, nil, nil)...)
	}

	// Port row (#2808's seam): the env factory and the meter read one walk —
	// under the switch the pod's bearer is refused, and nothing is metered;
	// the probe read the crossed bearer as a channel, the arm walks it as the
	// container does.
	add(sides(Row{
		Name:    "the subscription switch beside an ambient bearer",
		Origin:  "port of a2f5b9dfa onto #2808's env seam (anthropicEnvCredentials)",
		Posture: map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_AUTH_TOKEN": PodBearer, "ANTHROPIC_BASE_URL": gatewayURL},
		Node:    claw(""),
		Want:    Refuses(SubscriptionSwitch),
	}, nil, nil)...)

	// Port row (#2244): the engine's client sends no ambient header line, so
	// a header-bearing pod spends its key.
	add(sides(Row{
		Name:    "a header-bearing pod",
		Origin:  "port of 09ddb1695 onto #2244 (NoAmbientHeaders), commit 08f0ff152",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic, "ANTHROPIC_CUSTOM_HEADERS": "X-Tenant: a\nAuthorization: " + HeaderBearer},
		Node:    claw(""),
		Want:    Spent(0, PodEnv, "", PodAnthropic),
	}, nil, nil)...)

	// claw sends a key alone where a bearer sits beside it.
	add(sides(Row{
		Name:    "a key beside a bearer",
		Origin:  "bench 09ddb1695 TestAmbientEnvMeter_NamesTheCredentialClawSends",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic, "ANTHROPIC_AUTH_TOKEN": PodBearer},
		Node:    claw(""),
		Want:    Spent(0, PodEnv, "", PodAnthropic),
	}, nil, nil)...)

	// THE BEHAVIOUR CHANGE (ADR-161 decision 4): a stamped run's sealed key
	// serves its supervisor — ADR-131 §7 refuses the pod's environment only.
	// The prefix rule (AmbientSurvivingSpecs) dropped every non-gateway
	// supervisor of a stamped run; U2's filter keeps the specs the arm funds.
	for _, r := range stampings(Row{
		Name:    "a supervisor on the run's own key beside the pod's",
		Origin:  "ADR-161 decision 4 (AmbientSurvivingSpecs' prefix rule goes)",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderAnthropic: RunAnthropic}},
		Node:    claw(""),
		Role:    SupervisorRole,
		Trust:   Trusted,
		Want:    Spent(0, RunKey, string(secrets.ProviderAnthropic), RunAnthropic),
	}, nil, nil) {
		add(r)
	}
	// The stamp still refuses the pod's money to a supervisor: dropped at
	// spawn, warned at the launch, not metered.
	add(Row{
		Name:    "a stamped run's supervisor its run does not fund",
		Origin:  "ADR-161 decision 4 (the spawn keeps the specs Resolve funds, warning on the rest)",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Stamped: true},
		Node:    claw(""),
		Role:    SupervisorRole,
		Trust:   Trusted,
		Want:    Refuses(AmbientStamp),
	})

	// kimi, grok and opencode read their own configuration, never a run
	// credential (ADR-161 decision 5): their arm only refuses, under the
	// stamp; otherwise it answers own config — admitted with a warning, and
	// keyed as before (not metered).
	for _, own := range []struct{ article, backend, model string }{
		{"a", "kimi", "kimi-k2"},
		{"a", "grok", "grok-4"},
		{"an", "opencode", claudeSpec},
	} {
		for _, r := range stampings(Row{
			Name:    own.article + " " + own.backend + " node reading its own config",
			Origin:  "ADR-161 decision 5 (#108: the stamp binds every backend; own-config arms registered in U2)",
			Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic, "MOONSHOT_API_KEY": "moonshot-pod", "XAI_API_KEY": "xai-pod"},
			Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderAnthropic: RunAnthropic}},
			Node:    onBackend(own.backend, own.model),
			Trust:   Trusted,
			Want:    Spent(0, OwnConfig, "", ""),
		}, nil, func(r *Row) { r.Want = Refuses(AmbientStamp) }) {
			add(r)
		}
	}

	// The interim guard (ADR-161 decision 9, #111): until their arms land
	// (U4), a stamped pi or codex leg is refused — at the launch and at the
	// dispatch — where unguarded it spends the pod's environment.
	add(Row{
		Name:    "a stamped pi node beside the pod's key",
		Origin:  "ADR-161 decision 9 (the guard's witness: a stamped pi row)",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Stamped: true},
		Node:    onBackend("pi", claudeSpec),
		Trust:   Trusted,
		Want:    Refuses(ArmPending),
	})
	add(Row{
		Name:    "a stamped codex node beside the pod's key",
		Origin:  "ADR-161 decision 9 (the guard's witness: a stamped codex row)",
		Posture: map[string]string{"OPENAI_API_KEY": PodOpenAI},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Stamped: true},
		Node:    onBackend("codex", "openai/gpt-5.4"),
		Trust:   Trusted,
		Want:    Refuses(ArmPending),
	})

	for i := range rows {
		if rows[i].Role == "" {
			rows[i].Role = NodeRole
		}
	}
	return rows
}
