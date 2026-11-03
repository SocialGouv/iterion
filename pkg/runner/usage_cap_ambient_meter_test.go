package runner

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// ambientSourceFP renders the routing label the delegate stamps on a session
// that inherited an ambient credential. Spelled here rather than imported
// (the renderer is unexported) because it is the WIRE SHAPE this package
// reads: if the delegate and the runner ever disagree on the spelling, the
// rows below are what says so.
func ambientSourceFP(fp string) string {
	return "anthropic-env:" + fp
}

// The ambient label keys on the PLATFORM meter under the fingerprint the
// delegate computed: the pod's key is ONE account for the whole deployment,
// so what one tenant's run measures must reach every other borrower's
// pre-flight — and a node's own key, another fingerprint, never merges with
// the pod key it sits beside. Keying it on the run's scope would open one
// ledger per borrower of the same account — the split usageCapCredKeys
// exists to prevent for shared credentials.
func TestUsageCapCredKeys_TheAmbientLabelIsOnePlatformAccount(t *testing.T) {
	label := ambientSourceFP("fp-pod")
	// The label must be one the delegate actually answers, or this bench is
	// measuring its own spelling rather than the meter.
	if _, ok := delegate.AmbientSourceFingerprint(label); !ok {
		t.Fatalf("inert bench: AmbientSourceFingerprint(%q) does not parse", label)
	}
	want := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-pod")
	teamA := usageCapCredKeys(secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderZAI: "zai-team-a"},
		Fingerprints: map[string]string{string(secrets.ProviderZAI): "fp-zai-a"},
	}), &queue.RunMessage{TenantID: "team-a"})
	teamB := usageCapCredKeys(secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderZAI: "zai-team-b"},
		Fingerprints: map[string]string{string(secrets.ProviderZAI): "fp-zai-b"},
	}), &queue.RunMessage{TenantID: "team-b"})

	if got := teamA.forSource(label); got != want {
		t.Errorf("forSource(%q) = %q, want the platform meter %q — the pod's key is one account for every tenant", label, got, want)
	}
	if got := teamB.forSource(label); got != want {
		t.Errorf("team B reads %q where team A wrote — one account, one meter, want %q", got, want)
	}
	// The base URL a label may carry is routing, not the account.
	suffixed := ambientSourceFP("fp-pod") + ":https://gateway.internal/anthropic"
	if got := teamA.forSource(suffixed); got != want {
		t.Errorf("forSource(%q) = %q, want %q — the URL is routing, not the account", suffixed, got, want)
	}
	// A node's own key is another account: another fingerprint, another
	// meter, never merged with the pod's.
	nodeKey := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-node")
	if got := teamA.forSource(ambientSourceFP("fp-node")); got != nodeKey {
		t.Errorf("forSource(node label) = %q, want %q", got, nodeKey)
	}
	if nodeKey == want {
		t.Fatal("inert bench: the pod and the node fingerprints collided")
	}
	// The bare label stays the credential-less meter of the run's scope.
	if got, wantScope := teamA.forSource("anthropic-env"), usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-a"), ""); got != wantScope {
		t.Errorf("forSource(anthropic-env) = %q, want %q", got, wantScope)
	}
}

// The whole walk on the real label path: the run holds a tenant z.ai key,
// one of its nodes pins `provider: anthropic` and inherits the pod's
// ANTHROPIC_API_KEY, tenant A's session spends it and publishes its wall —
// and tenant B's pre-flight, walking the same pinned route, reads A's
// refusal and parks. One account, one meter, wherever the borrower sits.
func TestTheAmbientMeterCarriesAWallAcrossTenants(t *testing.T) {
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ZAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"ANTHROPIC_CUSTOM_HEADERS",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	creds := secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderZAI: "zai-team"},
		Fingerprints: map[string]string{string(secrets.ProviderZAI): "fp-zai"},
	}
	ctx := secrets.WithCredentials(context.Background(), creds)
	// The `anthropic` pin with no Anthropic key in the bundle is the route
	// that spends the pod's env: the delegate names the account it inherits.
	label, refused := delegate.AnthropicRouteSource(ctx, "anthropic")
	if refused {
		t.Fatal("the anthropic-pinned route refused — the pod's ambient key is spendable on an unstamped run")
	}
	if want := ambientSourceFP(secrets.FingerprintSHA256("sk-pod")); label != want {
		t.Fatalf("the delegate labeled the pod-env route %q, want %q", label, want)
	}

	store := usagecap.NewMemStore()
	teamA := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-a"})
	rejected := usagecap.Reading{
		Window:      usagecap.WindowFiveHour,
		Utilization: 1,
		Status:      usagecap.StatusRejected,
		ObservedAt:  time.Now().UTC(),
	}
	if err := store.Record(context.Background(), teamA.forSource(label), rejected); err != nil {
		t.Fatal(err)
	}

	teamB := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})
	route := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaudeCode, Hint: "anthropic", Readable: true}
	key, metered := teamB.routeKey(ctx, route)
	if !metered {
		t.Fatal("the pod-env route meters nothing — the pre-flight would sail past the wall tenant A measured")
	}
	readings, err := store.Latest(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	pol := usagecap.Policy{FiveHour: usagecap.WindowPolicy{MaxPercent: 80, Mode: usagecap.ModeSoft}}
	if d := usagecap.Preflight(readings, pol, time.Now().UTC(), usagecap.DefaultTrust()); !d.Blocked {
		t.Fatalf("tenant B's pre-flight read %d readings and did not block — the ambient wall did not cross tenants", len(readings))
	}
}

// The spend ledger reads the same labels: a fingerprinted ambient label
// names the inherited env — no slot of the run — and must not fall through
// to the (backend, model) booking, which would charge the pod's or a node's
// own spend to the bundle default the session never touched.
func TestSlotOfSource_TheFingerprintedAmbientLabelNamesNoSlot(t *testing.T) {
	creds := secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "zai-platform"}}
	label := ambientSourceFP("fp-pod")
	slot, named := slotOfSource(label)
	if !named || slot != "" {
		t.Errorf("slotOfSource(%q) = %q, %v — want named, no slot", label, slot, named)
	}
	if got := routeSlot(creds, routeKey{backend: delegate.BackendClaudeCode, source: label}); got != "" {
		t.Errorf("routeSlot booked the ambient spend on %q — want nobody", got)
	}
}

// The NOTHING-HELD default reads the ledger the coming session writes. A run
// holding no credential spends the pod's ambient one — a supervisor walking
// the registry's order into "else the pod's ambient env", an unreadable route
// keeping the run default — and since claw supervisors report no readings,
// the ambient account's refusal readings are the only evidence that account
// ever produces. They land on the fingerprinted ambient label (the test above
// pins the label's meter), so the pre-flight reading the bare no-credential
// meter would sail past every wall that account ever raises. On the
// ambient-forfait deployment — supervisor nodes and unreadable routes
// launching into a walled pod account — that is the whole pre-flight blind.
func TestTheNothingHeldDefaultReadsTheAmbientLedger(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	want := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, secrets.FingerprintSHA256("sk-pod"))

	// The default-precedence session names the pod's account, and the label
	// it stamps keys on THAT meter — the one the pre-flight must read.
	label, refused := delegate.AnthropicRouteSource(secrets.WithCredentials(context.Background(), secrets.Credentials{}), "")
	if refused {
		t.Fatal("the hintless default refused — the pod's ambient key is spendable on an unstamped run")
	}
	if _, ok := delegate.AmbientSourceFingerprint(label); !ok {
		t.Fatalf("the default-precedence session stamps %q, which names no ambient account", label)
	}

	// Tenant B holds nothing: its supervisor and its unreadable routes spend
	// the pod's env, and their pre-flight reads the account's own meter.
	ctxB := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	keysB := usageCapCredKeys(ctxB, &queue.RunMessage{TenantID: "team-b"})
	if got := keysB.defaultKey(); got != want {
		t.Errorf("the nothing-held run default = %q, want the pod account's meter %q", got, want)
	}
	route := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaudeCode, Readable: false}
	if key, metered := keysB.routeKey(ctxB, route); !metered || key != want {
		t.Errorf("the unreadable route = %q metered=%v, want %q — it keeps the run default, and the default spends the pod's env", key, metered, want)
	}
	supKey, metered := keysB.supervisorKey(ctxB, "anthropic/claude-sonnet-4")
	if !metered || supKey != want {
		t.Errorf("the supervisor = %q metered=%v, want %q — its own doc spends the pod's ambient env when nothing is held", supKey, metered, want)
	}

	// End to end across tenants: the wall tenant A measured on the ambient
	// account parks tenant B's supervisor, which spends the same account.
	store := usagecap.NewMemStore()
	ctxA := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderZAI: "zai-team-a"},
		Fingerprints: map[string]string{string(secrets.ProviderZAI): "fp-zai-a"},
	})
	if err := store.Record(context.Background(), usageCapCredKeys(ctxA, &queue.RunMessage{TenantID: "team-a"}).forSource(label), usagecap.Reading{
		Window:      usagecap.WindowFiveHour,
		Utilization: 1,
		Status:      usagecap.StatusRejected,
		ObservedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	readings, err := store.Latest(context.Background(), supKey)
	if err != nil {
		t.Fatal(err)
	}
	pol := usagecap.Policy{FiveHour: usagecap.WindowPolicy{MaxPercent: 80, Mode: usagecap.ModeSoft}}
	if d := usagecap.Preflight(readings, pol, time.Now().UTC(), usagecap.DefaultTrust()); !d.Blocked {
		t.Fatalf("tenant B's supervisor pre-flight read %d readings and did not block — the ambient wall it would spend past reached no meter it reads", len(readings))
	}
}

// The veto and the bare pod: a STAMPED run answers every ambient fallback
// with a typed refusal — its nothing-held default spends nothing of the pod's
// and stays on the no-credential meter — and a pod with no ambient credential
// has no account to read, so its meter stays bare too.
func TestTheNothingHeldDefaultKeepsTheBareMeterWithoutAnAmbientSpend(t *testing.T) {
	blankAnthropicWireEnv(t)
	bare := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "")
	msg := &queue.RunMessage{TenantID: "team-b"}

	stamped := usageCapCredKeys(secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true}), msg)
	key, metered := stamped.supervisorKey(context.Background(), "anthropic/claude-sonnet-4")
	if !metered || key != bare {
		t.Errorf("a stamped run's supervisor = %q metered=%v, want the bare %q — the run refuses the pod env, so its default spends nothing of it", key, metered, bare)
	}

	keys := usageCapCredKeys(secrets.WithCredentials(context.Background(), secrets.Credentials{}), msg)
	if got := keys.defaultKey(); got != bare {
		t.Errorf("a pod with no ambient credential = %q, want the bare %q — no inherited account names the spend", got, bare)
	}
}

// The nothing-held meter judges only the shapes the ambient account actually
// funds. A claw route on the anthropic wire and a hint-less supervisor
// `anthropic/…` spec walk the registry's order into the pod's ambient env —
// its meter is where their walls land. Everything else with nothing held
// funds an account whose walls never land there — opencode its own config, a
// gateway spec the deployment's OPENAI_COMPATIBLE pair, a zai/moonshot spec
// that provider's own env (zai: refused outright, the registry has no factory
// for it), a GLM id headed to api.anthropic.com on this pod (no base URL: the
// provider refuses the id before any account wall), a claw openai id the
// pod's openai env — and keying any of them on the ambient meter parks the
// run on a wall another vendor's account raised. Metering side and reading
// side must agree: the reading side books every one of these shapes on
// nobody.
func TestTheNothingHeldMeterJudgesOnlyTheShapesTheAmbientAccountFunds(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	want := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, secrets.FingerprintSHA256("sk-pod"))
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	keys := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})

	// Control: the shapes the ambient account funds keep its meter.
	if key, metered := keys.supervisorKey(ctx, "anthropic/claude-sonnet-4"); !metered || key != want {
		t.Errorf("the anthropic supervisor = %q metered=%v, want the ambient meter — its spec walks into the pod's env", key, metered)
	}
	clawAnthropic := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaw, Model: "claude-sonnet-4"}
	if key, metered := keys.routeKey(ctx, clawAnthropic); !metered || key != want {
		t.Errorf("the claw anthropic route = %q metered=%v, want the ambient meter — its spawn inherits the pod's env", key, metered)
	}
	// pi reads providers from the env allowlist it is handed, the pod's
	// ANTHROPIC_API_KEY included, so a hint naming anthropic funds from the
	// ambient account too.
	piAnthropic := model.WireRoute{NodeID: "n7", Backend: delegate.BackendPi, Hint: "anthropic", Model: "claude-opus-5-5", Readable: true}
	if key, metered := keys.routeKey(ctx, piAnthropic); !metered || key != want {
		t.Errorf("the pi anthropic route = %q metered=%v, want the ambient meter — pi's env allowlist resolves the pod's key", key, metered)
	}

	// The shapes funded elsewhere: not metered, whatever the ambient
	// account's meter says.
	for _, tc := range []struct {
		name  string
		key   func() (string, bool)
		call  string
		funds string
	}{
		{"zai supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "zai/glm-4.6") }, "supervisorKey(zai/glm-4.6)", "the registry has no zai factory — the spec is refused"},
		{"moonshot supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "moonshot/kimi-k2") }, "supervisorKey(moonshot/kimi-k2)", "the registry reads MOONSHOT_API_KEY"},
		{"gateway supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "openai_compatible/glm-5.3") }, "supervisorKey(openai_compatible/glm-5.3)", "the registry reads the deployment's gateway pair"},
		{"GLM supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "anthropic/glm-5.3") }, "supervisorKey(anthropic/glm-5.3)", "no base URL: api.anthropic.com refuses the id before any account wall"},
		{"openai supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "openai/gpt-6") }, "supervisorKey(openai/gpt-6)", "the registry reads OPENAI_API_KEY"},
		{"opencode route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n2", Backend: delegate.BackendOpenCode, Model: "anthropic/claude-opus-5-5", Readable: true})
		}, "routeKey(opencode)", "opencode resolves its own config"},
		{"claw GLM route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n3", Backend: delegate.BackendClaw, Model: "anthropic/glm-5.3", Readable: true})
		}, "routeKey(claw glm)", "no base URL: api.anthropic.com refuses the id before any account wall"},
		{"claw openai route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n4", Backend: delegate.BackendClaw, Model: "openai/gpt-6", Readable: true})
		}, "routeKey(claw openai)", "the pod's openai env funds the id"},
		{"gateway route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n5", Backend: delegate.BackendClaw, Model: "openai_compatible/glm-5.3", Readable: true})
		}, "routeKey(gateway)", "the deployment's gateway pair books on nobody"},
		{"zai-hinted pi route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n6", Backend: delegate.BackendPi, Hint: "zai", Model: "glm-5.3", Readable: true})
		}, "routeKey(pi zai)", "the z.ai key funds the hint"},
	} {
		if key, metered := tc.key(); metered {
			t.Errorf("%s = %q metered=%v, want not metered — %s, an account whose walls never land on the ambient meter", tc.call, key, metered, tc.funds)
		}
	}
}

// The ambient gateway funds a GLM id. The ambient-forfait deployment —
// ANTHROPIC_BASE_URL at a z.ai-compatible gateway beside an ambient
// ANTHROPIC_AUTH_TOKEN — walks a GLM id through the env factory exactly like
// a non-GLM one (the token rides as the request's bearer to the operator's
// gateway), so the ambient meter is where its walls land: a GLM supervisor
// spec and a claw GLM route park on it like any other shape the account
// funds. Where the pod's ZAI env is live the synthesis intercepts and z.ai's
// account pays — not this meter; where the factory drops the credential the
// id would ride (a z.ai/bigmodel base URL keeps no bearer, api.anthropic.com
// serves no GLM id) nothing spends and nothing is metered.
func TestTheAmbientGatewayFundsAGLMID(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://llm-gw.example.test/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-pod-gateway")
	want := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, secrets.FingerprintSHA256("tok-pod-gateway"))
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	keys := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})

	if key, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3"); !metered || key != want {
		t.Errorf("the GLM supervisor on the ambient gateway = %q metered=%v, want the ambient meter — the env factory serves the id from the pod's token", key, metered)
	}
	clawGLM := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaw, Model: "anthropic/glm-5.3", Readable: true}
	if key, metered := keys.routeKey(ctx, clawGLM); !metered || key != want {
		t.Errorf("the claw GLM route on the ambient gateway = %q metered=%v, want the ambient meter — its spec resolves from the pod's token", key, metered)
	}

	// The synthesis intercepts where the pod's ZAI env is live: z.ai's
	// account, whose walls never land on this meter.
	t.Setenv("ZAI_API_KEY", "zai-pod")
	if _, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3"); metered {
		t.Error("metered a GLM supervisor on a pod whose ZAI env funds the synthesis — another vendor's account")
	}
	if _, metered := keys.routeKey(ctx, clawGLM); metered {
		t.Error("metered a claw GLM route on a pod whose ZAI env funds the synthesis — another vendor's account")
	}
	t.Setenv("ZAI_API_KEY", "")

	// api.anthropic.com does not serve the id: no base URL, no account wall
	// whatever credential the pod carries.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	if _, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3"); metered {
		t.Error("metered a GLM supervisor headed to api.anthropic.com — the provider refuses the id before any account wall")
	}

	// On the vendor's own hostnames the factory drops the bearer: the client
	// resolves credential-less and no account spends.
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
	if _, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3"); metered {
		t.Error("metered a GLM supervisor on a z.ai base URL — the factory drops the bearer, no credential rides")
	}
}

// End to end: the capped ambient wall parks only the run that would spend the
// ambient account. The opencode route and the zai supervisor launch past it —
// under a blanket nothing-held metering they would park on evidence no spend
// of theirs can ever raise — while the claw anthropic route on the same pod
// still stops.
func TestUsageCapPreflight_TheAmbientWallParksOnlyTheRoutesThatSpendIt(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	caps := usagecap.NewMemStore()
	weekCapped(t, caps, platformKey(secrets.FingerprintSHA256("sk-pod")), time.Now().UTC().Add(30*time.Hour))

	opencode := chainWorkflow(agentRoute("oc", delegate.BackendOpenCode, "", "anthropic/claude-opus-5-5"))
	if err := preflightFor(t, caps, secrets.WithCredentials(context.Background(), secrets.Credentials{}), opencode); err != nil {
		t.Errorf("parked an opencode route on the ambient wall: %v — opencode's spend is its own config's answer", err)
	}

	clawAnthropic := chainWorkflow(agentRoute("impl", delegate.BackendClaw, "", "claude-sonnet-4"))
	if err := preflightFor(t, caps, secrets.WithCredentials(context.Background(), secrets.Credentials{}), clawAnthropic); err == nil {
		t.Error("started a claw anthropic route on the pod's capped ambient credential — the account its spawn inherits")
	}

	// The supervisor rows ride an impl node that never crosses the wall, so
	// the park (or its absence) names the SUPERVISOR's own metering.
	supervised := func(model string) *ir.Workflow {
		wf := chainWorkflow(agentRoute("impl", delegate.BackendOpenCode, "", "anthropic/claude-opus-5-5"))
		wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: model}}
		return wf
	}
	if err := preflightWithSupervisors(t, caps, secrets.WithCredentials(context.Background(), secrets.Credentials{}), supervised("zai/glm-4.6"), ""); err != nil {
		t.Errorf("parked a zai-pinned supervisor on the ambient wall: %v — the registry refuses the spec, it spends nothing", err)
	}
	if err := preflightWithSupervisors(t, caps, secrets.WithCredentials(context.Background(), secrets.Credentials{}), supervised("anthropic/claude-opus-5-5"), ""); err == nil {
		t.Error("started an anthropic-pinned supervisor on the pod's capped ambient credential")
	}
}

// End to end on the gateway pod: the capped ambient wall stops the GLM
// supervisor whose spec the env factory funds from the same token, and
// launches the same spec past the wall once the pod's ZAI env funds the
// synthesis instead.
func TestUsageCapPreflight_TheAmbientGatewayWallStopsAGLMSupervisor(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://llm-gw.example.test/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-pod-gateway-e2e")
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	caps := usagecap.NewMemStore()
	weekCapped(t, caps, platformKey(secrets.FingerprintSHA256("tok-pod-gateway-e2e")), time.Now().UTC().Add(30*time.Hour))

	supervised := func(model string) *ir.Workflow {
		wf := chainWorkflow(agentRoute("impl", delegate.BackendOpenCode, "", "anthropic/claude-opus-5-5"))
		wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: model}}
		return wf
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	if err := preflightWithSupervisors(t, caps, ctx, supervised("anthropic/glm-5.3"), ""); err == nil {
		t.Error("started a GLM supervisor on the pod's capped ambient gateway credential — the factory serves the id from that token")
	}
	t.Setenv("ZAI_API_KEY", "zai-pod-e2e")
	if err := preflightWithSupervisors(t, caps, ctx, supervised("anthropic/glm-5.3"), ""); err != nil {
		t.Errorf("parked a GLM supervisor on the ambient wall with the pod's ZAI env live: %v — the synthesis funds the id from z.ai's account", err)
	}
}
