package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/supervise"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// sandboxedClawSpend is the claw arm's answer for a sandboxed leg on spec:
// what the crossing hands the container and the container's walk lands on.
func sandboxedClawSpend(ctx context.Context, spec string) (credroute.Spend, error) {
	return credroute.Resolve(ctx, credroute.Call{
		Backend: delegate.BackendClaw, Spec: spec, Role: credroute.RoleNode,
		Sandbox: credroute.Sandbox{Contained: true},
	})
}

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
	route := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaudeCode, Legs: []credroute.Leg{{Provider: "anthropic"}}, Readable: true}
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
	route := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaudeCode, Legs: []credroute.Leg{{}}, Readable: false}
	if key, metered := keysB.routeKey(ctxB, route); !metered || key != want {
		t.Errorf("the unreadable route = %q metered=%v, want %q — it keeps the run default, and the default spends the pod's env", key, metered, want)
	}
	supKey, metered := keysB.supervisorKey(ctxB, "anthropic/claude-sonnet-4", "")
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

// The veto, on a pod that CARRIES a credential: a STAMPED run's claude_code
// and claw lanes answer every fallback to the pod env with a typed refusal, so
// the pod's account decides none of their meters (pi reads no stamp: its
// meter names the account pi spends). Two guards hold that, one scenario
// each. Its nothing-held default — an unreadable route, an unlabelled
// reading — stays on the no-credential meter, never the account's (the stamp
// read in usageCapCredKeys). A claw walk into the pod env, a supervisor's or a
// route's, is refused before any client exists — by the registry in process,
// by the crossing for the container — and meters nothing (the arm's refusal).
// The unstamped twin on the same pod reads the account on every row, so the
// stamp alone decides each one.
func TestAStampedRunReadsNothingOfThePodsAccount(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	ambient := platformKey(secrets.FingerprintSHA256("sk-pod"))
	bare := platformKey("")
	msg := &queue.RunMessage{TenantID: "team-b"}
	clawRoute := model.WireRoute{NodeID: "impl", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/claude-sonnet-4", Readable: true}
	unreadable := model.WireRoute{NodeID: "cc", Backend: delegate.BackendClaudeCode, Legs: []credroute.Leg{{}}, Readable: false}
	const supSpec = "anthropic/claude-sonnet-4"

	unstampedCtx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	twin := usageCapCredKeys(unstampedCtx, msg)
	if got := twin.defaultKey(); got != ambient {
		t.Fatalf("inert bench: the unstamped default on this pod = %q, want the account's meter %q", got, ambient)
	}
	if key, metered := twin.supervisorKey(unstampedCtx, supSpec, ""); !metered || key != ambient {
		t.Fatalf("inert bench: the unstamped supervisor = %q metered=%v, want the account's meter %q", key, metered, ambient)
	}
	for _, sandboxed := range []bool{false, true} {
		twin.sandboxed = sandboxed
		if key, metered := twin.routeKey(unstampedCtx, clawRoute); !metered || key != ambient {
			t.Fatalf("inert bench: the unstamped claw route (sandboxed=%v) = %q metered=%v, want the account's meter %q", sandboxed, key, metered, ambient)
		}
	}
	if _, err := sandboxedClawSpend(unstampedCtx, clawRoute.Model); err != nil {
		t.Fatalf("inert bench: the unstamped crossing hands the container no credential on a pod that carries one: %v", err)
	}

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	stamped := usageCapCredKeys(ctx, msg)

	// The nothing-held default: the bare meter, on both sides of the ledger.
	if got := stamped.defaultKey(); got != bare {
		t.Errorf("a stamped run's default = %q, want the bare %q — the run refuses the pod env, its default spends nothing of that account", got, bare)
	}
	if got := usageCapKey(ctx, msg); got != bare {
		t.Errorf("a stamped run's pre-flight default = %q, want the bare %q", got, bare)
	}
	if key, metered := stamped.routeKey(ctx, unreadable); !metered || key != bare {
		t.Errorf("a stamped run's unreadable route = %q metered=%v, want the bare %q", key, metered, bare)
	}
	if got := stamped.forSource(""); got != bare {
		t.Errorf("a stamped run's unlabelled reading = %q, want the bare %q", got, bare)
	}

	// The claw walk: in process the registry answers the typed refusal before
	// any client exists, and in the container's stead the crossing hands it
	// none of the pod's credentials and refuses the dispatch — so the meter
	// names nothing.
	var refusal *model.ErrAmbientCredentialRefused
	if _, err := model.NewRegistry().ResolveWithContext(ctx, supSpec); !errors.As(err, &refusal) {
		t.Fatalf("bench: the registry on a stamped run = %v, want the typed ambient refusal", err)
	}
	if spend, err := sandboxedClawSpend(ctx, clawRoute.Model); err == nil {
		t.Fatalf("bench: the crossing hands a stamped run's container the %s — the pod's account would fund the claw route", spend.Channel)
	}
	if key, metered := stamped.supervisorKey(ctx, supSpec, ""); metered {
		t.Errorf("a stamped run's supervisor = %q metered — its walk into the pod env is refused, no account spends", key)
	}
	for _, sandboxed := range []bool{false, true} {
		stamped.sandboxed = sandboxed
		if key, metered := stamped.routeKey(ctx, clawRoute); metered {
			t.Errorf("a stamped run's claw route (sandboxed=%v) = %q metered — its walk into the pod env is refused, no account spends", sandboxed, key)
		}
	}

	// End to end, through the pre-flight the runner calls. A wall on the
	// account alone parks the unstamped default and not the stamped one; a
	// wall on the account and on the bare meter parks no stamped claw run.
	resets := time.Now().UTC().Add(30 * time.Hour)
	unreadableRun := chainWorkflow(agentRoute("cc", delegate.BackendClaudeCode, "", "{{vars.model}}"))
	clawRun := chainWorkflow(agentRoute("impl", delegate.BackendClaw, "", "anthropic/claude-sonnet-4"))
	accountWall := usagecap.NewMemStore()
	weekCapped(t, accountWall, ambient, resets)
	if err := preflightFor(t, accountWall, unstampedCtx, unreadableRun); err == nil {
		t.Fatal("bench: an unstamped unreadable route started on the capped account its default spends")
	}
	if err := preflightFor(t, accountWall, ctx, unreadableRun); err != nil {
		t.Errorf("parked a stamped run's unreadable route on the pod account's wall: %v — its default spends nothing of that account", err)
	}
	bothWalls := usagecap.NewMemStore()
	weekCapped(t, bothWalls, ambient, resets)
	weekCapped(t, bothWalls, bare, resets)
	if err := preflightFor(t, bothWalls, unstampedCtx, clawRun); err == nil {
		t.Fatal("bench: an unstamped claw route started on the capped account it spends")
	}
	if err := preflightFor(t, bothWalls, ctx, clawRun); err != nil {
		t.Errorf("parked a stamped claw run: %v — its walk into the pod env is refused, no wall stands in its way", err)
	}
}

// The stamp refuses the pod env, never the run's own key. In process, a GLM
// id with no z.ai key of the run walks past the z.ai branch to the keyed
// factory, which spends the run's Anthropic key at the env's destination — a
// gateway here, which can serve the id — so the registry builds the client:
// the stamp's refusal comes after that arm, and so does the meter's. The
// supervisor and the claw route meter the run's key, not nothing, and not
// the pod's account beside it; a wall on that key parks the claw run.
func TestAStampedRunMetersTheOwnKeyItsClawWalkSpends(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example/anthropic")
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	const spec = "anthropic/glm-5.3"
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		AmbientRefused: true,
		APIKeys:        map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
		Fingerprints:   map[string]string{string(secrets.ProviderAnthropic): "fp-run"},
	})
	// The message preflightFor hands the pre-flight, so both read one scope.
	keys := usageCapCredKeys(ctx, &queue.RunMessage{RunID: "run-routes"})
	want := keys.keyForSlot(string(secrets.ProviderAnthropic))
	if want == keys.keyForSlot("") || want == platformKey(secrets.FingerprintSHA256("sk-pod")) {
		t.Fatalf("inert bench: the run key's meter %q is the bare one or the pod account's", want)
	}
	route := model.WireRoute{NodeID: "impl", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: spec, Readable: true}
	hint := model.RouteProviderHint(ctx, route.Backend, route.Legs[0].Provider, route.Model)
	if _, err := model.NewRegistry().ResolveWithContext(model.WithProviderHint(ctx, hint), spec); err != nil {
		t.Fatalf("bench: the registry refused a stamped run's own key on %s: %v", spec, err)
	}

	if key, metered := keys.supervisorKey(ctx, spec, ""); !metered || key != want {
		t.Errorf("a stamped run's supervisor on %s = %q metered=%v, want the run key's meter %q — the keyed factory spends it", spec, key, metered, want)
	}
	if key, metered := keys.routeKey(ctx, route); !metered || key != want {
		t.Errorf("a stamped run's claw route on %s = %q metered=%v, want the run key's meter %q", spec, key, metered, want)
	}
	caps := usagecap.NewMemStore()
	weekCapped(t, caps, want, time.Now().UTC().Add(30*time.Hour))
	if err := preflightFor(t, caps, ctx, chainWorkflow(agentRoute("impl", delegate.BackendClaw, "", spec))); err == nil {
		t.Error("a stamped claw run started on the capped key its walk spends")
	}
}

// A pod with no ambient credential has no account to read: the nothing-held
// default stays on the bare meter.
func TestTheNothingHeldDefaultOfABarePodIsTheBareMeter(t *testing.T) {
	blankAnthropicWireEnv(t)
	keys := usageCapCredKeys(secrets.WithCredentials(context.Background(), secrets.Credentials{}), &queue.RunMessage{TenantID: "team-b"})
	if got, bare := keys.defaultKey(), platformKey(""); got != bare {
		t.Errorf("a pod with no ambient credential = %q, want the bare %q — no inherited account names the spend", got, bare)
	}
}

// The nothing-held meter judges only the shapes the ambient account actually
// funds. A claw route on the anthropic wire and a hint-less supervisor
// `anthropic/…` spec walk the registry's order into the pod's ambient env —
// its meter is where their walls land. Everything else with nothing held
// funds an account whose walls never land there — opencode its own config
// (keyed on the run's unslotted meter, as before the resolver: ADR-161
// decision 5), a gateway spec the deployment's OPENAI_COMPATIBLE pair, a
// zai/moonshot spec
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
	if key, metered := keys.supervisorKey(ctx, "anthropic/claude-sonnet-4", ""); !metered || key != want {
		t.Errorf("the anthropic supervisor = %q metered=%v, want the ambient meter — its spec walks into the pod's env", key, metered)
	}
	// Readable, as the walk renders a claw spec it reads: an unreadable route
	// takes the run default instead and never reaches the claw arm.
	clawAnthropic := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/claude-sonnet-4", Readable: true}
	if key, metered := keys.routeKey(ctx, clawAnthropic); !metered || key != want {
		t.Errorf("the claw anthropic route = %q metered=%v, want the ambient meter — its spawn inherits the pod's env", key, metered)
	}
	// pi reads providers from the env allowlist it is handed, the pod's
	// ANTHROPIC_API_KEY included, so a hint naming anthropic funds from the
	// ambient account too.
	piAnthropic := model.WireRoute{NodeID: "n7", Backend: delegate.BackendPi, Legs: []credroute.Leg{{Provider: "anthropic"}}, Model: "claude-opus-5-5", Readable: true}
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
		{"zai supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "zai/glm-4.6", "") }, "supervisorKey(zai/glm-4.6)", "the registry has no zai factory — the spec is refused"},
		{"moonshot supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "moonshot/kimi-k2", "") }, "supervisorKey(moonshot/kimi-k2)", "the registry reads MOONSHOT_API_KEY"},
		{"gateway supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "openai_compatible/glm-5.3", "") }, "supervisorKey(openai_compatible/glm-5.3)", "the registry reads the deployment's gateway pair"},
		{"GLM supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "anthropic/glm-5.3", "") }, "supervisorKey(anthropic/glm-5.3)", "no base URL: api.anthropic.com refuses the id before any account wall"},
		{"openai supervisor", func() (string, bool) { return keys.supervisorKey(ctx, "openai/gpt-6", "") }, "supervisorKey(openai/gpt-6)", "the registry reads OPENAI_API_KEY"},
		{"claw GLM route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n3", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/glm-5.3", Readable: true})
		}, "routeKey(claw glm)", "no base URL: api.anthropic.com refuses the id before any account wall"},
		{"claw openai route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n4", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "openai/gpt-6", Readable: true})
		}, "routeKey(claw openai)", "the pod's openai env funds the id"},
		{"gateway route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n5", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "openai_compatible/glm-5.3", Readable: true})
		}, "routeKey(gateway)", "the deployment's gateway pair books on nobody"},
		{"zai-hinted pi route", func() (string, bool) {
			return keys.routeKey(ctx, model.WireRoute{NodeID: "n6", Backend: delegate.BackendPi, Legs: []credroute.Leg{{Provider: "zai"}}, Model: "glm-5.3", Readable: true})
		}, "routeKey(pi zai)", "the z.ai key funds the hint"},
	} {
		if key, metered := tc.key(); metered {
			t.Errorf("%s = %q metered=%v, want not metered — %s, an account whose walls never land on the ambient meter", tc.call, key, metered, tc.funds)
		}
	}
	// opencode resolves its own config: never the ambient account's meter,
	// and keyed where it was before the resolver (ADR-161 decision 5) — the
	// run's unslotted meter.
	opencode := model.WireRoute{NodeID: "n2", Backend: delegate.BackendOpenCode, Legs: []credroute.Leg{{}}, Model: "anthropic/claude-opus-5-5", Readable: true}
	if key, metered := keys.routeKey(ctx, opencode); !metered || key != keys.keyForSlot("") {
		t.Errorf("routeKey(opencode) = %q metered=%v, want the run's unslotted meter %q — keyed as before the resolver, never on the ambient account", key, metered, keys.keyForSlot(""))
	}
}

// The pi lane names ONE meter on both sides. pi resolves its providers from
// the env allowlist it is handed, the pod's ANTHROPIC_API_KEY included, so a
// route with an anthropic hint and no credential of the run spends the pod's
// ambient account — the pre-flight reads that account's meter for the shape
// (the pi row above) — and the refusal pi records must land on the SAME
// meter, or the wall one pi run measures reaches no pi pre-flight ever: the
// account's pi evidence would be dead letters and the next identical run
// starts and spends past it, the per-borrower split this file exists to keep
// closed, reopened between the two sides of one lane.
func TestThePiLaneNamesOneMeterOnBothSides(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ambient := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, secrets.FingerprintSHA256("sk-pod"))
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	keys := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})

	// The refusal pi stamps (the resolution its Execute names the refusal
	// with — one decision, both sides) must parse as an account and book on
	// the meter the pre-flight reads.
	label, metered, anthropic := delegate.PiAnthropicRouteSource(ctx, "claude-opus-5-5", "anthropic", false)
	if !anthropic || !metered {
		t.Fatalf("the pi route answered anthropic=%v metered=%v, want a metered anthropic route", anthropic, metered)
	}
	if _, ok := delegate.AmbientSourceFingerprint(label); !ok {
		t.Fatalf("the pi refusal stamps %q, which names no account at all", label)
	}
	if got := keys.forSource(label); got != ambient {
		t.Fatalf("the pi refusal books on %q, want the pod account's meter %q — no pre-flight reads the meter it wrote", got, ambient)
	}
	piRoute := model.WireRoute{NodeID: "n7", Backend: delegate.BackendPi, Legs: []credroute.Leg{{Provider: "anthropic"}}, Model: "claude-opus-5-5", Readable: true}
	if key, metered := keys.routeKey(ctx, piRoute); !metered || key != ambient {
		t.Fatalf("the pi route pre-flight = %q metered=%v, want %q", key, metered, ambient)
	}

	// End to end across tenants: the wall tenant A's pi run measured parks
	// tenant B's identical pi route.
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
	readings, err := store.Latest(context.Background(), ambient)
	if err != nil {
		t.Fatal(err)
	}
	pol := usagecap.Policy{FiveHour: usagecap.WindowPolicy{MaxPercent: 80, Mode: usagecap.ModeSoft}}
	if d := usagecap.Preflight(readings, pol, time.Now().UTC(), usagecap.DefaultTrust()); !d.Blocked {
		t.Fatalf("tenant B's pi pre-flight read %d readings and did not block — the wall the pi lane measured reached no meter it reads", len(readings))
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

	if key, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3", ""); !metered || key != want {
		t.Errorf("the GLM supervisor on the ambient gateway = %q metered=%v, want the ambient meter — the env factory serves the id from the pod's token", key, metered)
	}
	clawGLM := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/glm-5.3", Readable: true}
	if key, metered := keys.routeKey(ctx, clawGLM); !metered || key != want {
		t.Errorf("the claw GLM route on the ambient gateway = %q metered=%v, want the ambient meter — its spec resolves from the pod's token", key, metered)
	}

	// The synthesis intercepts where the pod's ZAI env is live: z.ai's
	// account, whose walls never land on this meter.
	t.Setenv("ZAI_API_KEY", "zai-pod")
	if _, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3", ""); metered {
		t.Error("metered a GLM supervisor on a pod whose ZAI env funds the synthesis — another vendor's account")
	}
	if _, metered := keys.routeKey(ctx, clawGLM); metered {
		t.Error("metered a claw GLM route on a pod whose ZAI env funds the synthesis — another vendor's account")
	}
	t.Setenv("ZAI_API_KEY", "")

	// api.anthropic.com does not serve the id: no base URL, no account wall
	// whatever credential the pod carries — and the same host named outright
	// is the same destination, the bearer or a key riding to it alike.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	if _, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3", ""); metered {
		t.Error("metered a GLM supervisor headed to api.anthropic.com — the provider refuses the id before any account wall")
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
	for _, key := range []string{"", "sk-pod-direct"} {
		t.Setenv("ANTHROPIC_API_KEY", key)
		if k, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3", ""); metered {
			t.Errorf("metered a GLM supervisor bound for api.anthropic.com named outright (API key %q) on %q — the destination serves no GLM id", key, k)
		}
		if k, metered := keys.routeKey(ctx, clawGLM); metered {
			t.Errorf("metered a claw GLM route bound for api.anthropic.com named outright (API key %q) on %q — the destination serves no GLM id", key, k)
		}
	}
	t.Setenv("ANTHROPIC_API_KEY", "")

	// On the vendor's own hostnames the factory drops the bearer: the client
	// resolves credential-less and no account spends.
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
	if _, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3", ""); metered {
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

	clawAnthropic := chainWorkflow(agentRoute("impl", delegate.BackendClaw, "", "anthropic/claude-sonnet-4"))
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

// The pi pre-flight reads the meter of the credential pi resolves — its own
// channel set, in its own order — never claude_code's reading of the pod. On
// the forfait pod (CLAUDE_CODE_OAUTH_TOKEN alone) pi holds nothing it can
// spend, so the forfait's capped wall parks a claude_code route and not a pi
// one; on a pod whose ANTHROPIC_OAUTH_TOKEN rides ahead of its API key, the
// pi route is judged on the OAuth token's meter, not on the key's that
// claude_code reads; and where pi's own store answers, no meter iterion keeps
// names the spend.
func TestThePiLaneReadsTheCredentialPiResolves(t *testing.T) {
	blankAnthropicWireEnv(t)
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	piNode := func() ir.Node { return agentRoute("pi", delegate.BackendPi, "anthropic", "claude-opus-5-5") }
	piRoute := model.WireRoute{NodeID: "pi", Backend: delegate.BackendPi, Legs: []credroute.Leg{{Provider: "anthropic"}}, Model: "claude-opus-5-5", Readable: true}

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-forfait")
	forfait := usagecap.NewMemStore()
	weekCapped(t, forfait, platformKey(secrets.FingerprintSHA256("sk-ant-oat-forfait")), time.Now().UTC().Add(30*time.Hour))
	if err := preflightFor(t, forfait, ctx, chainWorkflow(agentRoute("cc", delegate.BackendClaudeCode, "anthropic", "claude-opus-5-5"))); err == nil {
		t.Fatal("bench: a claude_code route started on the capped forfait it spends")
	}
	if err := preflightFor(t, forfait, ctx, chainWorkflow(piNode())); err != nil {
		t.Errorf("parked a pi route on the forfait's wall: %v — pi reads no CLAUDE_CODE_OAUTH_TOKEN", err)
	}

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "oat-pod")
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	keys := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})
	if key, metered := keys.routeKey(ctx, piRoute); !metered || key != platformKey(secrets.FingerprintSHA256("oat-pod")) {
		t.Errorf("the pi route = %q metered=%v, want the OAuth token's meter — pi sends it ahead of the API key", key, metered)
	}
	keyWall := usagecap.NewMemStore()
	weekCapped(t, keyWall, platformKey(secrets.FingerprintSHA256("sk-pod")), time.Now().UTC().Add(30*time.Hour))
	if err := preflightFor(t, keyWall, ctx, chainWorkflow(piNode())); err != nil {
		t.Errorf("parked a pi route on the API key's wall: %v — pi spends the OAuth token ahead of it", err)
	}
	tokenWall := usagecap.NewMemStore()
	weekCapped(t, tokenWall, platformKey(secrets.FingerprintSHA256("oat-pod")), time.Now().UTC().Add(30*time.Hour))
	if err := preflightFor(t, tokenWall, ctx, chainWorkflow(piNode())); err == nil {
		t.Error("started a pi route on the capped OAuth token it spends")
	}

	store := filepath.Join(os.Getenv("ITERION_PI_AGENT_DIR"), "auth.json")
	if err := os.WriteFile(store, []byte(`{"anthropic":{"type":"api_key","key":"sk-stored"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, metered := keys.routeKey(ctx, piRoute); metered {
		t.Errorf("the pi route = %q metered — pi's own store answers before every variable, and no meter iterion keeps names it", key)
	}
}

// Under ITERION_FORBID_SUBSCRIPTION_OAUTH the env factory refuses a walk that
// lands on a subscription bearer outright: no client is built and no account
// spends, so the nothing-held arms that mirror the factory — a supervisor
// spec, a claw route, GLM or not — are not judged on the ambient account's
// walls, and the typed refusal is their answer. A pod whose API key rides
// ahead of the bearer is spent, not refused, and stays metered — on the key's
// own account: claw sends the key alone, where the claude CLI's reading of the
// same pod names the key and the bearer together.
func TestTheNothingHeldMeterSkipsAWalkTheSwitchRefuses(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "https://llm-gw.example.test/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-pod-gateway")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	msg := &queue.RunMessage{TenantID: "team-b"}
	shapes := func(keys runCredKeys) map[string]func() (string, bool) {
		return map[string]func() (string, bool){
			"anthropic supervisor": func() (string, bool) { return keys.supervisorKey(ctx, "anthropic/claude-sonnet-4", "") },
			"GLM supervisor":       func() (string, bool) { return keys.supervisorKey(ctx, "anthropic/glm-5.3", "") },
			"claw anthropic route": func() (string, bool) {
				return keys.routeKey(ctx, model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/claude-sonnet-4", Readable: true})
			},
			"claw GLM route": func() (string, bool) {
				return keys.routeKey(ctx, model.WireRoute{NodeID: "n2", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/glm-5.3", Readable: true})
			},
		}
	}

	ambient := platformKey(secrets.FingerprintSHA256("tok-pod-gateway"))
	for name, key := range shapes(usageCapCredKeys(ctx, msg)) {
		if k, metered := key(); !metered || k != ambient {
			t.Errorf("switch off: the %s = %q metered=%v, want the ambient meter — the factory spends the pod's bearer", name, k, metered)
		}
	}

	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	for name, key := range shapes(usageCapCredKeys(ctx, msg)) {
		if k, metered := key(); metered {
			t.Errorf("switch on: the %s = %q metered — the factory refuses the bearer, no account spends", name, k)
		}
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-pod-gateway")
	keys := usageCapCredKeys(ctx, msg)
	keyAccount := platformKey(secrets.FingerprintSHA256("sk-pod-gateway"))
	if keys.nothingHeldKey() == keyAccount {
		t.Fatal("inert bench: the claude CLI's reading of this pod names the key alone")
	}
	for name, key := range shapes(keys) {
		if k, metered := key(); !metered || k != keyAccount {
			t.Errorf("switch on, an API key ahead: the %s = %q metered=%v, want the key's own meter %q — the factory spends the key alone, no refusal", name, k, metered, keyAccount)
		}
	}
}

// End to end: under the switch, a capped wall on the ambient account does not
// park a run whose only spender on that account is a supervisor the factory
// refuses — the supervisor's typed refusal surfaces when it evaluates — and
// the same run parks once the switch is off and the supervisor spends it.
func TestUsageCapPreflight_ARefusedSupervisorIsNotParkedOnTheAmbientWall(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-oat-pod-e2e")
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	caps := usagecap.NewMemStore()
	weekCapped(t, caps, platformKey(secrets.FingerprintSHA256("sk-ant-oat-pod-e2e")), time.Now().UTC().Add(30*time.Hour))
	wf := chainWorkflow(agentRoute("impl", delegate.BackendOpenCode, "", "anthropic/claude-opus-5-5"))
	wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "anthropic/claude-sonnet-4"}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})

	if err := preflightWithSupervisors(t, caps, ctx, wf, ""); err == nil {
		t.Fatal("bench: the switch off, a supervisor spending the capped ambient bearer started")
	}
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	if err := preflightWithSupervisors(t, caps, ctx, wf, ""); err != nil {
		t.Errorf("parked a run on the ambient wall of a bearer the factory refuses: %v", err)
	}
}

// The claw lane reads the credential claw's own resolution sends, never the
// claude CLI's reading of the pod. On the forfait pod (CLAUDE_CODE_OAUTH_TOKEN
// alone) claw holds nothing it can spend: a claw route and an anthropic
// supervisor key the no-credential meter, and the forfait's capped wall parks
// a claude_code route and neither of them. On a pod whose API key sits beside
// that token, claw sends the key alone: the key's own meter, which the claude
// CLI's reading — the key and the token together — never names, so a wall on
// that set does not park claw and a wall on the key does.
func TestTheClawLaneReadsTheCredentialClawSends(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	msg := &queue.RunMessage{TenantID: "team-b"}
	clawRoute := model.WireRoute{NodeID: "impl", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/claude-sonnet-4", Readable: true}
	clawRun := func() *ir.Workflow {
		return chainWorkflow(agentRoute("impl", delegate.BackendClaw, "", "anthropic/claude-sonnet-4"))
	}
	supervisedRun := func() *ir.Workflow {
		wf := chainWorkflow(agentRoute("impl", delegate.BackendOpenCode, "", "anthropic/claude-opus-5-5"))
		wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "anthropic/claude-sonnet-4"}}
		return wf
	}
	resets := time.Now().UTC().Add(30 * time.Hour)

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-forfait")
	forfait := platformKey(secrets.FingerprintSHA256("sk-ant-oat-forfait"))
	keys := usageCapCredKeys(ctx, msg)
	if keys.nothingHeldKey() != forfait {
		t.Fatalf("inert bench: the claude CLI's reading of the forfait pod is %q, want the forfait's meter", keys.nothingHeldKey())
	}
	// claw reads no CLAUDE_CODE_OAUTH_TOKEN: its call carries no credential,
	// the arm's no-credential refusal, which no wall meters (ADR-161
	// decision 7).
	if key, metered := keys.routeKey(ctx, clawRoute); metered {
		t.Errorf("the claw route on the forfait pod = %q metered — claw reads no CLAUDE_CODE_OAUTH_TOKEN, its call carries no credential", key)
	}
	if key, metered := keys.supervisorKey(ctx, "anthropic/claude-sonnet-4", ""); metered {
		t.Errorf("the anthropic supervisor on the forfait pod = %q metered — its call carries no credential", key)
	}
	forfaitWall := usagecap.NewMemStore()
	weekCapped(t, forfaitWall, forfait, resets)
	if err := preflightFor(t, forfaitWall, ctx, chainWorkflow(agentRoute("cc", delegate.BackendClaudeCode, "", "claude-opus-5-5"))); err == nil {
		t.Fatal("bench: a claude_code route started on the capped forfait it spends")
	}
	if err := preflightFor(t, forfaitWall, ctx, clawRun()); err != nil {
		t.Errorf("parked a claw route on the forfait's wall: %v — claw spends nothing of that account", err)
	}
	if err := preflightWithSupervisors(t, forfaitWall, ctx, supervisedRun(), ""); err != nil {
		t.Errorf("parked a run on the forfait's wall through its claw supervisor: %v — claw spends nothing of that account", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	keys = usageCapCredKeys(ctx, msg)
	keyAccount := platformKey(secrets.FingerprintSHA256("sk-pod"))
	setAccount := keys.nothingHeldKey()
	if setAccount == keyAccount {
		t.Fatal("inert bench: the claude CLI's reading names the key alone")
	}
	if key, metered := keys.routeKey(ctx, clawRoute); !metered || key != keyAccount {
		t.Errorf("the claw route beside the forfait token = %q metered=%v, want the key's meter %q — claw sends the key alone", key, metered, keyAccount)
	}
	if key, metered := keys.supervisorKey(ctx, "anthropic/claude-sonnet-4", ""); !metered || key != keyAccount {
		t.Errorf("the anthropic supervisor beside the forfait token = %q metered=%v, want the key's meter %q", key, metered, keyAccount)
	}
	setWall := usagecap.NewMemStore()
	weekCapped(t, setWall, setAccount, resets)
	if err := preflightFor(t, setWall, ctx, clawRun()); err != nil {
		t.Errorf("parked a claw route on the wall of the set the claude CLI sends: %v — claw sends the key alone", err)
	}
	keyWall := usagecap.NewMemStore()
	weekCapped(t, keyWall, keyAccount, resets)
	if err := preflightFor(t, keyWall, ctx, clawRun()); err == nil {
		t.Error("started a claw route on the capped key it spends")
	}
	if err := preflightWithSupervisors(t, keyWall, ctx, supervisedRun(), ""); err == nil {
		t.Error("started a run whose claw supervisor spends the capped key")
	}
}

// A claw walk the env factory refuses spends nothing: on a pod whose
// ZAI_API_KEY is its only credential claw reads, the synthesis refuses an id
// z.ai does not serve before any client exists — the typed refusal is the
// route's answer and its own fallbacks fire. No wall parks it, whichever meter
// the pod's other readings name.
func TestAClawWalkTheFactoryRefusesIsNotMetered(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ZAI_API_KEY", "zai-pod")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-forfait")
	if _, err := model.NewRegistry().Resolve("anthropic/claude-sonnet-4"); err == nil {
		t.Fatal("bench: the env factory resolved a claude id on the z.ai synthesis")
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	keys := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})
	clawRoute := model.WireRoute{NodeID: "impl", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/claude-sonnet-4", Readable: true}
	if key, metered := keys.routeKey(ctx, clawRoute); metered {
		t.Errorf("the claw route = %q metered — the factory refuses the walk, no account spends", key)
	}
	if key, metered := keys.supervisorKey(ctx, "anthropic/claude-sonnet-4", ""); metered {
		t.Errorf("the anthropic supervisor = %q metered — the factory refuses the walk, no account spends", key)
	}

	walled := usagecap.NewMemStore()
	resets := time.Now().UTC().Add(30 * time.Hour)
	weekCapped(t, walled, keys.nothingHeldKey(), resets)
	weekCapped(t, walled, platformKey(""), resets)
	if err := preflightFor(t, walled, ctx, chainWorkflow(agentRoute("impl", delegate.BackendClaw, "", "anthropic/claude-sonnet-4"))); err != nil {
		t.Errorf("parked a claw route the factory refuses: %v", err)
	}
	// The node beside the supervisor is one the pre-flight never meters (a
	// kimi node: its own config), routed `anthropic` so the supervisor
	// carries the anthropic family as its hint: the supervisor's walk alone
	// is judged.
	wf := chainWorkflow(agentRoute("impl", delegate.BackendKimi, "anthropic", "kimi-k2"))
	wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "anthropic/claude-sonnet-4"}}
	if err := preflightWithSupervisors(t, walled, ctx, wf, ""); err != nil {
		t.Errorf("parked a run on its supervisor's refused walk: %v", err)
	}
}

// An unpinned supervisor carries the watched nodes' provider family as its
// hint — `anthropic` beside an anthropic node — whatever model it then
// resolves (here the deployment's ITERION_DEFAULT_SUPERVISOR_MODEL), and claw
// honours that hint as a node's own: the direct path, the ambient key or this
// host's login sent to api.anthropic.com, the bearer and the base URL never
// read. On the ambient gateway pod (a bearer at a gateway, no key, no login)
// that path has nothing to spend and refuses: the spawn drops the supervisor
// by name (runview.FundedSupervisorSpecs), so the pre-flight does not park the
// run on the bearer's wall, which the pinned twin — no hint, the env path —
// does spend. And where the run holds a Claude forfait the direct path takes
// it on the direct wire, past a gateway base URL the env path stops at.
func TestTheSupervisorIsReadUnderTheHintItResolvesWith(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "anthropic/claude-sonnet-4")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://llm-gw.example.test/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-pod-gateway")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	msg := &queue.RunMessage{RunID: "run-hint", TenantID: "team-b"}
	supervised := func(pin string) *ir.Workflow {
		wf := chainWorkflow(agentRoute("impl", delegate.BackendOpenCode, "", "anthropic/claude-opus-5-5"))
		wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: pin}}
		return wf
	}
	specs := supervise.SpecsFromWorkflow(supervised(""), nil, model.ModelOverrides{}, nil)
	if len(specs) != 1 || specs[0].ProviderHint != "anthropic" {
		t.Fatalf("bench: the unpinned supervisor carries %+v, want the watched node's family as its hint — its evaluator resolves under it", specs)
	}
	resolved, err := supervise.ResolveModel(ctx, specs[0].Model, specs[0].ProviderHint)
	if err != nil || !strings.HasPrefix(resolved, "anthropic/") {
		t.Fatalf("bench: the unpinned supervisor resolved %q (%v), want the deployment default's anthropic spec", resolved, err)
	}
	keys := usageCapCredKeys(ctx, msg)
	if key, metered := keys.supervisorKey(ctx, resolved, specs[0].ProviderHint); metered {
		t.Errorf("the hinted supervisor = %q metered — the direct path has no key and no login, it refuses", key)
	}
	if sups := preflightSupervisors(ctx, supervised(""), msg, nil, nil, nil); len(sups) != 0 {
		t.Errorf("the walk keeps %+v — the spawn drops a supervisor no credential funds", sups)
	}
	walled := usagecap.NewMemStore()
	weekCapped(t, walled, platformKey(secrets.FingerprintSHA256("tok-pod-gateway")), time.Now().UTC().Add(30*time.Hour))
	if err := preflightWithSupervisors(t, walled, ctx, supervised("anthropic/claude-sonnet-4"), ""); err == nil {
		t.Fatal("bench: the pinned supervisor, which spends the bearer, started on its wall")
	}
	if err := preflightWithSupervisors(t, walled, ctx, supervised(""), ""); err != nil {
		t.Errorf("parked a run on the bearer's wall through a supervisor that never sends it: %v", err)
	}

	forfait := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): sealedForfaitDir(t)},
		PlatformSourced:      map[string]bool{string(secrets.OAuthKindClaudeCode): true},
		Fingerprints:         map[string]string{string(secrets.OAuthKindClaudeCode): "fp-forfait"},
	})
	fkeys := usageCapCredKeys(forfait, msg)
	if key, metered := fkeys.supervisorKey(forfait, "anthropic/claude-sonnet-4", "anthropic"); !metered || key != platformKey("fp-forfait") {
		t.Errorf("the hinted supervisor beside the run's forfait = %q metered=%v, want the forfait's meter — the direct path takes it on the direct wire", key, metered)
	}
	if key, _ := fkeys.supervisorKey(forfait, "anthropic/claude-sonnet-4", ""); key == platformKey("fp-forfait") {
		t.Fatal("inert bench: the unhinted env path takes the forfait past the gateway base URL too")
	}
}

// A sandboxed pi node reads the container's agent dir, and no host bind
// carries this host's ~/.pi into it: a login in the host's default store
// answers nothing there, the session spends the channels forwarded into the
// container, and the pre-flight reads that account's meter. On the host the
// same store answers first and no meter iterion keeps names the spend.
func TestTheSandboxedPiRouteReadsTheContainersStore(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_PI_AGENT_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "auth.json"), []byte(`{"anthropic":{"type":"api_key","key":"sk-stored"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	podKey := platformKey(secrets.FingerprintSHA256("sk-pod"))
	piRoute := model.WireRoute{NodeID: "pi", Backend: delegate.BackendPi, Legs: []credroute.Leg{{Provider: "anthropic"}}, Model: "claude-opus-5-5", Readable: true}

	keys := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})
	if key, metered := keys.routeKey(ctx, piRoute); metered {
		t.Fatalf("bench: the host pi route = %q metered — the host's store answers first", key)
	}
	keys.sandboxed = true
	if key, metered := keys.routeKey(ctx, piRoute); !metered || key != podKey {
		t.Errorf("the sandboxed pi route = %q metered=%v, want the forwarded key's meter %q — the container never sees the host's store", key, metered, podKey)
	}

	walled := usagecap.NewMemStore()
	weekCapped(t, walled, podKey, time.Now().UTC().Add(30*time.Hour))
	piRun := chainWorkflow(agentRoute("pi", delegate.BackendPi, "anthropic", "claude-opus-5-5"))
	host := capRunner(capTestPolicy(), walled, &capStatusStore{})
	if err := host.usageCapPreflight(ctx, piRun, &queue.RunMessage{RunID: "run-pi"}, iterlog.Nop()); err != nil {
		t.Fatalf("bench: an unsandboxed pi run parked on the key its host store shadows: %v", err)
	}
	sandboxed := capRunner(capTestPolicy(), walled, &capStatusStore{})
	sandboxed.cfg.SandboxDefault = "auto"
	if err := sandboxed.usageCapPreflight(ctx, piRun, &queue.RunMessage{RunID: "run-pi"}, iterlog.Nop()); err == nil {
		t.Error("started a sandboxed pi run on the capped key the container spends")
	}
}

// A GLM id the run holds no z.ai key for is not a nothing-held spec when the
// run holds its own Anthropic key: claw's in-process resolution passes its z.ai
// branch by and spends that key (the keyed factory, at the env's
// ANTHROPIC_BASE_URL), so a claw supervisor and an in-process claw route are
// judged on the run key's meter — the tenant's walls, not the pod account's,
// which the call never spends. In the container the crossing pins a GLM node on
// z.ai and carries no Anthropic key, so the sandboxed route spends the pod env
// and keeps that meter. Where the destination is the real Anthropic API the id
// is refused before any wall, whatever key rides, and nothing is metered.
func TestAGLMIDSpendsTheRunsAnthropicKeyInProcess(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gw.example.test/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-pod-gateway")
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-run"},
	})
	runKey := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("t"), "fp-run")
	podKey := platformKey(secrets.FingerprintSHA256("tok-pod-gateway"))

	client, err := model.NewRegistry().ResolveWithContext(ctx, "anthropic/glm-5.3")
	if err != nil {
		t.Fatalf("bench: the registry refused the GLM id: %v", err)
	}
	if got := reflect.ValueOf(client).Elem().FieldByName("APIKey").String(); got != "sk-run" {
		t.Fatalf("bench: the registry built the GLM client on %q, want the run's own key", got)
	}

	keys := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "t"})
	clawGLM := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/glm-5.3", Readable: true}
	if key, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3", ""); !metered || key != runKey {
		t.Errorf("the GLM supervisor = %q metered=%v, want the run key's meter %q — the registry spends it, not the pod's token", key, metered, runKey)
	}
	if key, metered := keys.routeKey(ctx, clawGLM); !metered || key != runKey {
		t.Errorf("the in-process claw GLM route = %q metered=%v, want the run key's meter %q", key, metered, runKey)
	}
	keys.sandboxed = true
	if key, metered := keys.routeKey(ctx, clawGLM); !metered || key != podKey {
		t.Errorf("the sandboxed claw GLM route = %q metered=%v, want the pod account's meter %q — the crossing carries no Anthropic key for a GLM node", key, metered, podKey)
	}
	keys.sandboxed = false

	for _, base := range []string{"", "https://api.anthropic.com"} {
		t.Setenv("ANTHROPIC_BASE_URL", base)
		if key, metered := keys.supervisorKey(ctx, "anthropic/glm-5.3", ""); metered {
			t.Errorf("base %q: metered the GLM supervisor on %q — api.anthropic.com serves no GLM id, whatever key rides", base, key)
		}
		if key, metered := keys.routeKey(ctx, clawGLM); metered {
			t.Errorf("base %q: metered the in-process claw GLM route on %q — api.anthropic.com serves no GLM id", base, key)
		}
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://gw.example.test/anthropic")

	// End to end: the run key's wall parks the run through its claw supervisor,
	// the pod account's wall does not.
	supervised := func() *ir.Workflow {
		wf := chainWorkflow(agentRoute("impl", delegate.BackendOpenCode, "", "anthropic/claude-opus-5-5"))
		wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "anthropic/glm-5.3"}}
		return wf
	}
	resets := time.Now().UTC().Add(30 * time.Hour)
	runWall := usagecap.NewMemStore()
	weekCapped(t, runWall, platformKey("fp-run"), resets) // the pre-flight's message names no tenant
	if err := preflightWithSupervisors(t, runWall, ctx, supervised(), ""); err == nil {
		t.Error("started a run whose claw GLM supervisor spends the capped run key")
	}
	podWall := usagecap.NewMemStore()
	weekCapped(t, podWall, podKey, resets)
	if err := preflightWithSupervisors(t, podWall, ctx, supervised(), ""); err != nil {
		t.Errorf("parked a run on the pod account's wall through a supervisor that spends the run's key: %v", err)
	}
}

// One account, one meter, on the lane whose client applies the custom-header
// block — and the env's own account on every lane whose client does not. A
// header line named after a header the Anthropic wire's auth sets replaces
// what that auth put there for the claude CLI: an Authorization line the
// bearer, an x-api-key line the key, so the claude_code route (the label its
// session stamps) keys the header's account. claw's engine client refuses the
// ambient block (withClientIdentity sets NoAmbientHeaders) and the sandbox
// crossing strips its credential lines, so a claw route sends the env value
// itself — pi's account, which reads no header block either. A wall the
// claude_code sessions record on the header account parks no claw route; a
// wall on the env account does.
func TestAHeaderAccountIsMeteredOnlyOnTheLaneThatSendsIt(t *testing.T) {
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	ccRoute := model.WireRoute{NodeID: "cc", Backend: delegate.BackendClaudeCode, Legs: []credroute.Leg{{}}, Model: "claude-opus-5-5", Readable: true}
	clawRoute := model.WireRoute{NodeID: "claw", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: "anthropic/claude-sonnet-4", Readable: true}
	piRoute := model.WireRoute{NodeID: "pi", Backend: delegate.BackendPi, Legs: []credroute.Leg{{Provider: "anthropic"}}, Model: "claude-opus-5-5", Readable: true}
	for _, tc := range []struct {
		name     string
		env      map[string]string
		header   string // the credential the claude CLI's request carries
		envSpend string // the env value claw and pi send
	}{
		{"an Authorization line over the pod's bearer", map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok-pod", "ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer hdr-pod"}, "Bearer hdr-pod", "tok-pod"},
		{"an x-api-key line over the pod's key", map[string]string{"ANTHROPIC_API_KEY": "sk-pod", "ANTHROPIC_CUSTOM_HEADERS": "x-api-key: hdr-key"}, "hdr-key", "sk-pod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blankAnthropicWireEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			keys := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})
			headerKey := platformKey(secrets.FingerprintSHA256(tc.header))
			envKey := platformKey(secrets.FingerprintSHA256(tc.envSpend))
			cc, ccMetered := keys.routeKey(ctx, ccRoute)
			claw, clawMetered := keys.routeKey(ctx, clawRoute)
			if !ccMetered || cc != headerKey {
				t.Errorf("the claude_code route = %q metered=%v, want the header account's meter %q", cc, ccMetered, headerKey)
			}
			if !clawMetered || claw != envKey {
				t.Errorf("the claw route = %q metered=%v, want the env account's meter %q — claw's client sends no ambient header line", claw, clawMetered, envKey)
			}
			if pi, piMetered := keys.routeKey(ctx, piRoute); !piMetered || pi != envKey {
				t.Errorf("the pi route = %q metered=%v, want the meter of the env value pi sends %q — pi reads no header block", pi, piMetered, envKey)
			}

			// End to end: the header account's wall parks no claw route, the
			// env account's does.
			resets := time.Now().UTC().Add(30 * time.Hour)
			headerWall := usagecap.NewMemStore()
			weekCapped(t, headerWall, headerKey, resets)
			if err := preflightFor(t, headerWall, ctx, chainWorkflow(agentRoute("claw", delegate.BackendClaw, "", "anthropic/claude-sonnet-4"))); err != nil {
				t.Errorf("parked a claw route on the header account its request never carries: %v", err)
			}
			envWall := usagecap.NewMemStore()
			weekCapped(t, envWall, envKey, resets)
			if err := preflightFor(t, envWall, ctx, chainWorkflow(agentRoute("claw", delegate.BackendClaw, "", "anthropic/claude-sonnet-4"))); err == nil {
				t.Error("started a claw route on the capped env account it sends")
			}
		})
	}
}

// An unlabelled reading — a binary that predates the source stamp — is charged
// to the run's default, the meter the pre-flight reads for that same default
// (defaultKey, usageCapKey): on a run holding nothing, the pod's ambient
// account. On the bare meter instead, the wall it records lands where no
// pre-flight of a nothing-held run on that pod ever reads. An unrecognised
// label names no route either and lands with it; the bare "anthropic-env" — a
// session that SAID no inherited credential named its spend — keeps the bare
// meter. A run holding a legacy forfait record (no fingerprint) holds a
// credential all the same: its default is that slot's fingerprint-less meter
// on both sides, never the pod's account.
func TestAnUnlabelledReadingLandsOnThePreflightsDefault(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ambient := platformKey(secrets.FingerprintSHA256("sk-pod"))
	msg := &queue.RunMessage{RunID: "run-unlabelled", TenantID: "team-b"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	keys := usageCapCredKeys(ctx, msg)
	if got := usageCapKey(ctx, msg); got != ambient {
		t.Fatalf("bench: the pre-flight default = %q, want the pod account's meter %q", got, ambient)
	}
	for _, source := range []string{"", "facade:https://gw.example.test/anthropic"} {
		if got := keys.forSource(source); got != ambient {
			t.Errorf("forSource(%q) = %q, want the pre-flight's default %q", source, got, ambient)
		}
	}
	if got, bare := keys.forSource("anthropic-env"), platformKey(""); got != bare {
		t.Errorf("forSource(anthropic-env) = %q, want the bare %q — the session said no inherited credential named its spend", got, bare)
	}

	// Through the recording side the runner wires: the reading the guard
	// publishes with no source lands on the meter the next pre-flight reads.
	caps := usagecap.NewMemStore()
	g := capRunner(capTestPolicy(), caps, nil).usageGuardFor(ctx, msg, iterlog.Nop())
	if g == nil {
		t.Fatal("want a guard")
	}
	g.Observe(usagecap.Reading{Window: usagecap.WindowFiveHour, Utilization: 0.5, ObservedAt: time.Now().UTC()})
	if got, err := caps.Latest(context.Background(), usageCapKey(ctx, msg)); err != nil || len(got) != 1 {
		t.Errorf("the pre-flight's default meter holds %d reading(s) (err=%v), want the unlabelled one", len(got), err)
	}

	legacy := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): t.TempDir()},
	})
	fpLess := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-b"), "")
	if got := usageCapKey(legacy, msg); got != fpLess {
		t.Errorf("a legacy forfait run's default = %q, want its fingerprint-less meter %q — the run spends its forfait, not the pod's account", got, fpLess)
	}
	if got := usageCapCredKeys(legacy, msg).forSource(""); got != fpLess {
		t.Errorf("a legacy forfait run's unlabelled reading = %q, want %q", got, fpLess)
	}
}

// A claw `anthropic/claude-*` route spends the run's Claude forfait past a
// z.ai key the run also holds: in process the registry resolves a claude id
// past the z.ai key to the forfait, and the crossing hands the container the
// forfait — the z.ai key rides a GLM node alone. So the pre-flight reads the
// forfait's meter and the ledger books the forfait, stamped or not, in
// process and sandboxed. Read on the z.ai key, a walled forfait starts the
// run, and a wall on the z.ai key parks claude runs that never touch it.
func TestAClawClaudeRouteMetersTheForfaitPastAZAIKey(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	t.Cleanup(model.SetOAuthDirLookup(model.RunOAuthDirLookup))
	forfait := t.TempDir()
	if err := os.WriteFile(filepath.Join(forfait, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-run","expiresAt":4102444800000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const spec = "anthropic/claude-sonnet-4"
	route := model.WireRoute{NodeID: "impl", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{}}, Model: spec, Readable: true}
	clawRun := chainWorkflow(agentRoute("impl", delegate.BackendClaw, "", spec))
	resets := time.Now().UTC().Add(30 * time.Hour)

	for _, row := range []struct {
		name    string
		stamped bool
	}{{"unstamped", false}, {"stamped", true}} {
		t.Run(row.name, func(t *testing.T) {
			creds := secrets.Credentials{
				AmbientRefused:       row.stamped,
				APIKeys:              map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"},
				OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfait},
				Fingerprints: map[string]string{
					string(secrets.ProviderZAI):         "fp-zai",
					string(secrets.OAuthKindClaudeCode): "fp-oauth",
				},
			}
			ctx := secrets.WithCredentials(context.Background(), creds)
			// The bench: in process the registry builds the client on the
			// forfait, and stamped — every credential of the pod withheld —
			// the crossing's first channel is the forfait's.
			client, err := model.NewRegistry().ResolveWithContext(ctx, spec)
			if err != nil {
				t.Fatalf("bench: the registry refused the route: %v", err)
			}
			if cc, ok := client.(*api.Client); !ok || cc.OAuthToken != "sk-ant-oat-run" {
				t.Fatalf("bench: the registry's client is %T, not one on the run's forfait", client)
			}
			if row.stamped {
				if spend, err := sandboxedClawSpend(ctx, spec); err != nil || spend.Channel != credroute.ChannelRunForfait {
					t.Fatalf("bench: the crossing's spend = %+v, %v, want the run's forfait", spend, err)
				}
			}
			// The message preflightFor hands the pre-flight, so both read one scope.
			keys := usageCapCredKeys(ctx, &queue.RunMessage{RunID: "run-routes"})
			want := keys.keyForSlot(string(secrets.OAuthKindClaudeCode))
			zaiKey := keys.keyForSlot(string(secrets.ProviderZAI))
			if want == zaiKey {
				t.Fatalf("inert bench: the forfait and the z.ai key share the meter %q", want)
			}

			for _, sandboxed := range []bool{false, true} {
				keys.sandboxed = sandboxed
				if key, metered := keys.routeKey(ctx, route); !metered || key != want {
					t.Errorf("the claw route (sandboxed=%v) = %q metered=%v, want the forfait's meter %q — the z.ai key funds no claude id", sandboxed, key, metered, want)
				}
			}
			if got := routeSlot(creds, routeKey{backend: delegate.BackendClaw, model: spec}); got != string(secrets.OAuthKindClaudeCode) {
				t.Errorf("the ledger books the claw route on %q, want the forfait the call spends", got)
			}

			forfaitWall := usagecap.NewMemStore()
			weekCapped(t, forfaitWall, want, resets)
			if err := preflightFor(t, forfaitWall, ctx, clawRun); err == nil {
				t.Error("the claw run started on the capped forfait it spends")
			}
			zaiWall := usagecap.NewMemStore()
			weekCapped(t, zaiWall, zaiKey, resets)
			if err := preflightFor(t, zaiWall, ctx, clawRun); err != nil {
				t.Errorf("parked the claw run on the z.ai key's wall: %v — the route never spends it", err)
			}
		})
	}
}

// A claw `anthropic/claude-*` route holding no Anthropic key spends the run's
// Claude forfait only where claw's walk under the route's hint spends it
// (model.ClawForfaitServes): behind a destination other than the real
// Anthropic wire, or under ITERION_FORBID_SUBSCRIPTION_OAUTH beside the pod's
// key, it spends the pod's account — in process and, the crossing reading the
// same walk, in the container. So the route's pre-flight reads that account's
// meter, as the supervisor meter does for the same spec, and the ledger books
// no slot of the run; a `provider: "anthropic"` hint sends the forfait direct
// past a redirected wire, and both meters read the forfait's.
func TestAClawClaudeRouteMetersWhatClawsWalkSpendsOffTheForfaitsWire(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	t.Cleanup(model.SetOAuthDirLookup(model.RunOAuthDirLookup))
	forfait := t.TempDir()
	if err := os.WriteFile(filepath.Join(forfait, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-run","expiresAt":4102444800000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const spec = "anthropic/claude-sonnet-4"
	creds := secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfait},
		Fingerprints: map[string]string{
			string(secrets.ProviderZAI):         "fp-zai",
			string(secrets.OAuthKindClaudeCode): "fp-oauth",
		},
	}
	ctx := secrets.WithCredentials(context.Background(), creds)
	podAccount := platformKey(secrets.FingerprintSHA256("sk-pod"))
	resets := time.Now().UTC().Add(30 * time.Hour)

	for _, row := range []struct {
		name, baseURL, forbid, hint string
		forfaitServes               bool
		// nodeRefused: the node dispatch's switch guard refuses the route
		// (ADR-161 parity row t50 r2 #1), so no wall meters it; the
		// supervisor's evaluator walks the registry alone and spends the
		// pod's key.
		nodeRefused bool
	}{
		{name: "a gateway wire", baseURL: "https://llm-gw.example.com"},
		{name: "the subscription switch beside the pod's key", forbid: "1", nodeRefused: true},
		{name: "hinted anthropic past a gateway wire", baseURL: "https://llm-gw.example.com", hint: "anthropic", forfaitServes: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_BASE_URL", row.baseURL)
			t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", row.forbid)
			t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
			// The bench: the account the in-process client carries.
			client, err := model.NewRegistry().ResolveWithContext(model.WithProviderHint(ctx, row.hint), spec)
			cc, ok := client.(*api.Client)
			if err != nil || !ok {
				t.Fatalf("bench: the registry answered %T err=%v", client, err)
			}
			keys := usageCapCredKeys(ctx, &queue.RunMessage{RunID: "run-routes"})
			forfaitKey := keys.keyForSlot(string(secrets.OAuthKindClaudeCode))
			want, other := podAccount, forfaitKey
			if row.forfaitServes {
				want, other = forfaitKey, podAccount
				if cc.OAuthToken != "sk-ant-oat-run" {
					t.Fatalf("bench: the registry's client carries none of the run's forfait")
				}
			} else if cc.APIKey != "sk-pod" || cc.OAuthToken != "" {
				t.Fatalf("bench: the registry's client is not on the pod's key")
			}
			if want == other {
				t.Fatalf("inert bench: the forfait and the pod's account share the meter %q", want)
			}

			route := model.WireRoute{NodeID: "impl", Backend: delegate.BackendClaw, Legs: []credroute.Leg{{Provider: row.hint}}, Model: spec, Readable: true}
			for _, sandboxed := range []bool{false, true} {
				keys.sandboxed = sandboxed
				key, metered := keys.routeKey(ctx, route)
				switch {
				case row.nodeRefused && metered:
					t.Errorf("the claw route (sandboxed=%v) = %q metered — the node's switch guard refuses it, no account spends", sandboxed, key)
				case !row.nodeRefused && (!metered || key != want):
					t.Errorf("the claw route (sandboxed=%v) = %q metered=%v, want %q — the account claw's walk spends", sandboxed, key, metered, want)
				}
			}
			keys.sandboxed = false
			if key, metered := keys.supervisorKey(ctx, spec, row.hint); !metered || key != want {
				t.Errorf("the supervisor meter of the same spec = %q metered=%v, want %q", key, metered, want)
			}
			if !row.forfaitServes {
				if got := routeSlot(creds, routeKey{backend: delegate.BackendClaw, model: spec}); got != "" {
					t.Errorf("the ledger books the claw route on %q, want no slot — the pod's account spent it", got)
				}
			}

			clawRun := chainWorkflow(agentRoute("impl", delegate.BackendClaw, row.hint, spec))
			wall := usagecap.NewMemStore()
			weekCapped(t, wall, want, resets)
			if err := preflightFor(t, wall, ctx, clawRun); row.nodeRefused && err != nil {
				t.Errorf("parked a claw run whose node the switch refuses on %q: %v — the node spends nothing", want, err)
			} else if !row.nodeRefused && err == nil {
				t.Error("the claw run started on the capped account it spends")
			}
			otherWall := usagecap.NewMemStore()
			weekCapped(t, otherWall, other, resets)
			if err := preflightFor(t, otherWall, ctx, clawRun); err != nil {
				t.Errorf("parked the claw run on a wall its route never meets: %v", err)
			}
		})
	}
}
