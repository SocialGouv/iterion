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

func pending(reader Reader, unit string, today Spend, why string) Pending {
	return Pending{Reader: reader, Unit: unit, Today: today, Why: why}
}

func with(r *Row, p ...Pending) { r.Pending = append(r.Pending, p...) }

// hintBlindLedger pins the ledger on a hinted claw node past a redirected
// wire: the route it books carries no hint, so it reads the hint-less walk,
// which keeps the forfait home, while the call spent it on the direct wire
// (deferred by the bench's t50 r1 fix; ADR-161 decision 8 has claw's events
// carry the Spend's slot).
func hintBlindLedger() Pending {
	return pending(Ledger, "U2", Nobody,
		"t50 r1 fix, deferred 1: the ledger's route carries no claw hint and books the hinted forfait spend on nobody")
}

func pins(r *Row, reader Reader) bool {
	_, p := r.Expect(reader)
	return p != nil
}

// todayAdmissionCode pins main's admission, which knows one code
// (SANDBOX_CREDENTIAL_UNREACHABLE): a refusal the ADR types otherwise.
func todayAdmissionCode(r *Row) {
	if !pins(r, Admission) && r.Judged() && r.Sandboxed && r.Want.Refused() && AdmissionCode(r.Want.Refusal) != CodeUnreachable {
		with(r, pending(Admission, "U1", Refuses(NoCredential),
			"main's admission refuses with SANDBOX_CREDENTIAL_UNREACHABLE alone; ADR-161 decision 7 types the stamp and the switch"))
	}
}

// todayHostStampUnjudged pins main's start sites: a stamped run on the host
// passes no admission (resolveAndStartSandbox returns before it).
func todayHostStampUnjudged(r *Row) {
	if !pins(r, Admission) && !r.Sandboxed && r.Run.Stamped && r.Want.Refused() {
		with(r, pending(Admission, "U1", Launches,
			"t53 r2 #3: an unsandboxed stamped start returns before admitSandboxCredentials"))
	}
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
	// run's forfait is judged there, and spends.
	for _, r := range stampings(Row{
		Name:    "a hinted Claude node past a z.ai wire",
		Origin:  "bench 6e2389644 TestAHintedClaudeNodeCrossesItsRunsForfaitPastARedirectedWire (t50 r1 #1)",
		Posture: map[string]string{"ANTHROPIC_BASE_URL": zaiWireURL, "ZAI_API_KEY": PodZAI},
		Run:     Run{Forfait: true},
		Node:    claw("anthropic", Leg{Provider: "anthropic"}),
		Want:    Spent(0, RunForfait, string(secrets.OAuthKindClaudeCode), ForfaitToken),
		Pending: []Pending{hintBlindLedger()},
	}, nil, nil) {
		add(sides(r, nil, nil)...)
	}

	// SEEN RED (t50 r2 #1): ClawBackend.Execute's switch guard refuses the
	// node before either arm, the pod's key untouched; the meter keys the
	// pod's account, and the probe, which never meets the guard, admits.
	for _, r := range stampings(Row{
		Name:    "the subscription switch beside the pod's key",
		Origin:  "t50 r2 #1 (open MEDIUM); bench 6e2389644 TestTheForbidSwitchCrossesWhatTheInProcessTwinSpends",
		Posture: map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Forfait: true},
		Node:    claw(""),
		Want:    Refuses(SubscriptionSwitch),
	}, func(r *Row) {
		with(r, pending(Meter, "U2", Spent(0, PodEnv, "", PodAnthropic),
			"t50 r2 #1: routeKey keys the pod's account for a node GuardSubscriptionOAuth refuses"))
	}, nil) {
		add(sides(r, nil, func(r *Row) {
			if !r.Run.Stamped {
				with(r, pending(Admission, "U1", Launches,
					"t50 r2 #1: the probe composes the crossing, which never meets Execute's guard, and admits"))
			}
		})...)
	}

	// SEEN RED (t50 r2 #2): the probe splits the hint on its first comma, raw;
	// dispatch expands and parses the chain, and crosses the run's forfait.
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
			Pending: []Pending{pending(Admission, "U1", Refuses(NoCredential),
				"t50 r2 #2: ClawSandboxCredential reads the field's raw first comma token, not the parsed chain"),
				hintBlindLedger()},
		})
	}

	// SEEN RED (the plan row, ADR-161 context): a pool-lent forfait the wire
	// keeps home — the call spends the pod's key, the plan books the donor.
	add(sides(Row{
		Name:    "a pool-lent forfait beside a gateway wire",
		Origin:  "ADR-161 context (plan ≠ dispatch, t53 r1 #1); #2288 258bc2fbf",
		Posture: map[string]string{"ANTHROPIC_BASE_URL": gatewayURL, "ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Forfait: true, Plan: true},
		Node:    claw(""),
		Want:    Spent(0, PodEnv, "", PodAnthropic),
		Pending: []Pending{pending(Plan, "U2", Spent(0, RunForfait, string(secrets.OAuthKindClaudeCode), ForfaitToken),
			"plan ≠ dispatch: the fine table joins the step's provider word to the publisher's pod-blind plan row")},
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
	// dies on the provider's refusal; the meter keys the scope's
	// no-credential meter where ADR-161 decision 7 reads a refusal as not
	// metered.
	add(sides(Row{
		Name:   "nothing of the run's nor the pod's",
		Origin: "#1768 TestForkRunOnClawWithoutCredentialChannelIsRefused; bench 09ddb1695 TestAmbientEnvMeter_NoEnvCredentialIsTheBareSpend",
		Node:   claw(""),
		Want:   Refuses(NoCredential),
		Pending: []Pending{pending(Meter, "U2", Spend{Channel: HostLogin},
			"AmbientEnvMeter answers metered with no fingerprint for a credential-less client: the bare meter, for a call no account pays")},
	}, nil, nil)...)

	// SEEN RED (t53 r2 #2): a stamped first leg refused, dispatch falls through
	// to the run's z.ai key; the pre-flight reads the first leg only.
	add(sides(Row{
		Name:    "a fall-through leg past a stamped first leg",
		Origin:  "t53 r2 #2 (ADR-161 review: a fall-through leg unmetered)",
		Posture: map[string]string{"ANTHROPIC_API_KEY": PodAnthropic},
		Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Stamped: true},
		Node:    claw("anthropic,zai:"+glmSpec, Leg{Provider: "anthropic"}, Leg{Provider: "zai", Model: glmSpec}),
		Want:    Spent(1, RunKey, string(secrets.ProviderZAI), RunZAI),
		Pending: []Pending{pending(Meter, "U2", Refuses(AmbientStamp),
			"t53 r2 #2: AnthropicWireRoutes keys the first chain element, refused, while dispatch spends leg 2")},
	}, nil, func(r *Row) {
		with(r, pending(Admission, "U1", Refuses(NoCredential),
			"the probe composes the first leg's crossing only, refused under the stamp, while leg 2 is funded"))
	})...)

	// SEEN RED (t53 r2 #4): the allowlist drops the leg the pod funds; the
	// probe, blind to it, admits on that leg.
	add(Row{
		Name:      "an allowlist-dropped first leg (sandboxed)",
		Origin:    "t53 r2 #4 (ADR-161 review: an allowlist-dropped leg admitted)",
		Posture:   map[string]string{"ZAI_API_KEY": PodZAI},
		Run:       Run{Allowlist: []string{"anthropic"}},
		Node:      claw("zai:"+glmSpec+",anthropic", Leg{Provider: "anthropic"}),
		Sandboxed: true,
		Trust:     Fork,
		Want:      Refuses(HintUnfunded),
		Pending: []Pending{pending(Admission, "U1", Launches,
			"t53 r2 #4: the probe reads the raw first leg the allowlist drops, and finds the pod's z.ai key crossing")},
	})

	// SEEN RED (t53 r2 #3): a stamped run on the host passes no admission.
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
	// passes no admission.
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
		Pending: []Pending{pending(Admission, "U1", Launches,
			"t53 r3 #2: startSandbox's adopt branch returns before resolveAndStartSandbox and its admission")},
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

	// Port row (#2808's seam): the env factory and the meter read one walk —
	// under the switch the pod's bearer is refused, and nothing is metered.
	add(sides(Row{
		Name:    "the subscription switch beside an ambient bearer",
		Origin:  "port of a2f5b9dfa onto #2808's env seam (anthropicEnvCredentials)",
		Posture: map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_AUTH_TOKEN": PodBearer, "ANTHROPIC_BASE_URL": gatewayURL},
		Node:    claw(""),
		Want:    Refuses(SubscriptionSwitch),
	}, nil, func(r *Row) {
		with(r, pending(Admission, "U1", Launches,
			"the probe reads the crossed ANTHROPIC_AUTH_TOKEN as a channel; the container's factory refuses it under the switch"))
	})...)

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

	for i := range rows {
		if rows[i].Role == "" {
			rows[i].Role = NodeRole
		}
		todayAdmissionCode(&rows[i])
		todayHostStampUnjudged(&rows[i])
	}
	return rows
}
