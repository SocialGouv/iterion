package forge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"maps"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The launch-vars pins an operator set on a repo — gate_context naming the
// required check, arm_automerge, mono_family — live on the integration AND on
// the webhook config that enforces them. A partial write (send one key, mean
// "change this one") used to REPLACE the whole map: every pin the caller did
// not echo was dropped silently, and the repo kept certifying a configuration
// that was no longer in force.

func provisionWithPins(t *testing.T, o *Orchestrator, connID string, pins map[string]string) ProvisionResult {
	t.Helper()
	res, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID:     "t1",
		ConnectionID: connID,
		RepoFullName: "group/api",
		BotIDs:       []string{"review-pr"},
		LaunchVars:   pins,
		ActorID:      "tester",
	})
	if err != nil {
		t.Fatalf("provision with pins: %v", err)
	}
	return res
}

func gatePins() map[string]string {
	return map[string]string{
		"gate_context":  "iterion/review",
		"arm_automerge": "true",
		"mono_family":   "revi",
	}
}

func readBothStores(t *testing.T, o *Orchestrator, res ProvisionResult) (RepoIntegration, webhooks.Config) {
	t.Helper()
	integ, err := o.Integrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatalf("read integration back: %v", err)
	}
	cfg, err := o.Webhooks.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatalf("read webhook config back: %v", err)
	}
	return integ, cfg
}

// A partial launch_vars write must never silently drop a pin: the write is
// refused, the refusal names every key the caller has to echo, and both stores
// keep enforcing the pins that were never mentioned.
func TestProvisionRefusesAPartialLaunchVarsDrop(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithPins(t, o, conn.ID, gatePins())

	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID:     "t1",
		ConnectionID: conn.ID,
		RepoFullName: "group/api",
		BotIDs:       []string{"review-pr"},
		LaunchVars:   map[string]string{"gate_context": "revi/review"},
		ActorID:      "tester",
		Replace:      true,
	})
	if !errors.Is(err, ErrLaunchVarsDrop) {
		t.Fatalf("partial write: err = %v, want ErrLaunchVarsDrop — the pins the caller did not echo were dropped silently", err)
	}
	for _, key := range []string{"arm_automerge", "mono_family"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the refusal must name the key the caller has to echo: %q missing from %q", key, err.Error())
		}
	}
	if !strings.Contains(err.Error(), "launch_vars_replace") {
		t.Errorf("the refusal must name the explicit escape hatch: %q", err.Error())
	}

	integ, cfg := readBothStores(t, o, res)
	for key, want := range gatePins() {
		if got := integ.LaunchVars[key]; got != want {
			t.Errorf("integration lost %s: got %q, want %q — a refused write must change nothing", key, got, want)
		}
		if got := cfg.OperatorLaunchVars[key]; got != want {
			t.Errorf("webhook config lost %s: got %q, want %q — the enforcement half must survive a refused write", key, got, want)
		}
	}
}

// Replacing the whole map stays possible — as an EXPLICIT gesture, never as
// the side effect of naming one key.
func TestProvisionLaunchVarsReplaceIsAnExplicitGesture(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithPins(t, o, conn.ID, gatePins())

	if _, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID:          "t1",
		ConnectionID:      conn.ID,
		RepoFullName:      "group/api",
		BotIDs:            []string{"review-pr"},
		LaunchVars:        map[string]string{"gate_context": "revi/review"},
		LaunchVarsReplace: true,
		ActorID:           "tester",
		Replace:           true,
	}); err != nil {
		t.Fatalf("explicit replace: %v", err)
	}
	integ, cfg := readBothStores(t, o, res)
	if len(integ.LaunchVars) != 1 || integ.LaunchVars["gate_context"] != "revi/review" {
		t.Errorf("integration after explicit replace: %v, want exactly gate_context=revi/review", integ.LaunchVars)
	}
	if len(cfg.OperatorLaunchVars) != 1 || cfg.OperatorLaunchVars["gate_context"] != "revi/review" {
		t.Errorf("config after explicit replace: %v, want exactly gate_context=revi/review", cfg.OperatorLaunchVars)
	}
}

// Echoing the stored keys while changing one is the documented switch: it is
// not a drop, so no replace flag is needed, and BOTH stores end on the new
// value — the integration is the report, the config the enforcement, and one
// without the other certifies a configuration that is not in force.
func TestProvisionLaunchVarsEchoSwitchWritesBothStores(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithPins(t, o, conn.ID, gatePins())

	echo := gatePins()
	echo["gate_context"] = "revi/review"
	if _, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID:     "t1",
		ConnectionID: conn.ID,
		RepoFullName: "group/api",
		BotIDs:       []string{"review-pr"},
		LaunchVars:   echo,
		ActorID:      "tester",
		Replace:      true,
	}); err != nil {
		t.Fatalf("echo switch: %v", err)
	}
	integ, cfg := readBothStores(t, o, res)
	for key, want := range echo {
		if got := integ.LaunchVars[key]; got != want {
			t.Errorf("integration %s = %q, want %q", key, got, want)
		}
		if got := cfg.OperatorLaunchVars[key]; got != want {
			t.Errorf("config %s = %q, want %q — the enforcement half was not switched", key, got, want)
		}
	}
}

// R3-1: key-absence in the config is AMBIGUOUS — a deliberate drop whose
// integration write crashed (ErrProvisionDiverged's shape) is indistinguishable
// from a stale integration predating the config — so no merge can adopt on a
// diverged pair without risking resurrecting a dropped pin from the stale
// store. The nil-map adopt REFUSES on divergence instead, naming the
// diverging keys; the converging gesture is an explicit launch_vars (or
// launch_vars_replace), exactly the idiom the other ErrProvisionDiverged
// sites use.
func TestProvisionNilMapAdoptRefusesOnDivergence(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithPins(t, o, conn.ID, gatePins())

	// The deliberate-drop crash shape: the operator replaced the whole map
	// with only gate_context; the config write landed, the integration's
	// never did — the integration still reports all three pins.
	integ, cfg := readBothStores(t, o, res)
	cfg.OperatorLaunchVars = map[string]string{"gate_context": "revi/review"}
	if err := o.Webhooks.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
	})
	if !errors.Is(err, ErrProvisionDiverged) {
		t.Fatalf("nil-map adopt over a diverged pair: err = %v, want ErrProvisionDiverged — the merge would have resurrected the dropped pins from the stale store", err)
	}
	for _, key := range []string{"gate_context", "arm_automerge", "mono_family"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the refusal must name the diverging key %q: %q", key, err.Error())
		}
	}
	// Nothing moved: the deliberate drop stays dropped, the stale report untouched.
	integ, cfg = readBothStores(t, o, res)
	if len(cfg.OperatorLaunchVars) != 1 {
		t.Errorf("the enforced (dropped) map was rewritten by a refused adopt: %v", cfg.OperatorLaunchVars)
	}
	if len(integ.LaunchVars) != 3 {
		t.Errorf("the stale report was rewritten by a refused adopt: %v", integ.LaunchVars)
	}

	// The converging gesture is the explicit one: launch_vars_replace with
	// the intended map. It lands on BOTH stores — the drop is honored, the
	// divergence closed.
	if _, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
		LaunchVars: map[string]string{"gate_context": "revi/review"}, LaunchVarsReplace: true,
	}); err != nil {
		t.Fatalf("explicit replace after the refusal: %v", err)
	}
	integ, cfg = readBothStores(t, o, res)
	if len(integ.LaunchVars) != 1 || integ.LaunchVars["gate_context"] != "revi/review" {
		t.Errorf("integration after the converging write: %v", integ.LaunchVars)
	}
	if len(cfg.OperatorLaunchVars) != 1 || cfg.OperatorLaunchVars["gate_context"] != "revi/review" {
		t.Errorf("config after the converging write: %v", cfg.OperatorLaunchVars)
	}
}

// The clear-to-empty crash shape diverges the same way: a deliberate
// clear-all whose integration write crashed must also refuse, not resurrect.
func TestProvisionNilMapAdoptRefusesAClearedConfigDivergence(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithPins(t, o, conn.ID, gatePins())

	integ, cfg := readBothStores(t, o, res)
	cfg.OperatorLaunchVars = nil // the clear-all landed on the config only
	if err := o.Webhooks.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(integ.LaunchVars) != 3 {
		t.Fatalf("precondition: the stale report keeps all three pins: %v", integ.LaunchVars)
	}

	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
	})
	if !errors.Is(err, ErrProvisionDiverged) {
		t.Fatalf("nil-map adopt over a cleared-config divergence: err = %v, want ErrProvisionDiverged", err)
	}
	for _, key := range []string{"gate_context", "arm_automerge", "mono_family"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the refusal must name the diverging key %q: %q", key, err.Error())
		}
	}
}

// TestProvisionNilMapAdoptUnchangedWhenStoresAgree is the equal-maps control:
// when both stores say the same thing — the normal path — the adopt yields
// exactly the stored map and the re-provision is the no-op it always was.
func TestProvisionNilMapAdoptUnchangedWhenStoresAgree(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithPins(t, o, conn.ID, gatePins())

	before, cfgBefore := readBothStores(t, o, res)
	out, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
	})
	if err != nil {
		t.Fatalf("nil-map re-provision: %v", err)
	}
	if out.Created {
		t.Error("a fully-idempotent re-provision reported created=true")
	}
	integ, cfg := readBothStores(t, o, res)
	for key, want := range gatePins() {
		if got := integ.LaunchVars[key]; got != want || cfg.OperatorLaunchVars[key] != want {
			t.Errorf("%s drifted on an equal-stores adopt: integ=%q cfg=%q", key, got, cfg.OperatorLaunchVars[key])
		}
	}
	if !maps.Equal(integ.LaunchVars, before.LaunchVars) || !maps.Equal(cfg.OperatorLaunchVars, cfgBefore.OperatorLaunchVars) {
		t.Errorf("the adopt rewrote maps that agreed: %v / %v", integ.LaunchVars, cfg.OperatorLaunchVars)
	}
}

// The refusal baseline is BOTH stores, not one: a crash between the
// config-first and integration-second writes (restart is normal, no error is
// ever returned) leaves the config pinning keys the integration lacks. A
// partial write echoing every INTEGRATION key would then pass a baseline that
// read only the integration and silently drop the config-only pin — and
// verifyOperatorSettings cannot catch it, because it compares against what
// was just written.
func TestProvisionRefusesAPartialWriteDroppingAConfigOnlyPin(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithPins(t, o, conn.ID, gatePins())

	// The diverged state a mid-write crash leaves: the config pins all three
	// keys, the integration only gate_context.
	integ, cfg := readBothStores(t, o, res)
	integ.LaunchVars = map[string]string{"gate_context": "iterion/review"}
	if err := o.Integrations.Update(context.Background(), integ); err != nil {
		t.Fatal(err)
	}
	if len(cfg.OperatorLaunchVars) != 3 {
		t.Fatalf("precondition: config pins %v", cfg.OperatorLaunchVars)
	}

	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID:     "t1",
		ConnectionID: conn.ID,
		RepoFullName: "group/api",
		BotIDs:       []string{"review-pr"},
		LaunchVars:   map[string]string{"gate_context": "revi/review"},
		ActorID:      "tester",
		Replace:      true,
	})
	if !errors.Is(err, ErrLaunchVarsDrop) {
		t.Fatalf("partial write over a diverged baseline: err = %v, want ErrLaunchVarsDrop — the config-only pins were dropped silently", err)
	}
	for _, key := range []string{"arm_automerge", "mono_family"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the refusal must name the config-only key %q: %q", key, err.Error())
		}
	}
	_, cfg = readBothStores(t, o, res)
	if got := cfg.OperatorLaunchVars["mono_family"]; got != "revi" {
		t.Errorf("config-only pin lost: mono_family = %q", got)
	}
}

// failOnceIntegrationStore fails the FIRST Update it is asked for: the crash
// between the two stores the launch-vars write crosses.
type failOnceIntegrationStore struct {
	*MemoryRepoIntegrationStore
	failNext bool
}

func (f *failOnceIntegrationStore) Update(ctx context.Context, ri RepoIntegration) error {
	if f.failNext {
		f.failNext = false
		return errors.New("store lost the write")
	}
	return f.MemoryRepoIntegrationStore.Update(ctx, ri)
}

// The operator-settings write crosses TWO stores with no shared transaction.
// A failure between them must (a) leave the ENFORCEMENT half already written —
// never a report certifying settings that are not in force — (b) fail loudly,
// and (c) converge when the same request is re-run, because Provision is
// idempotent.
func TestProvisionLaunchVarsPartialWriteConvergesOnRerun(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithPins(t, o, conn.ID, gatePins())

	failing := &failOnceIntegrationStore{MemoryRepoIntegrationStore: NewMemoryRepoIntegrationStore(), failNext: true}
	integ, err := o.Integrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := failing.Create(context.Background(), integ); err != nil {
		t.Fatal(err)
	}
	o.Integrations = failing

	echo := gatePins()
	echo["gate_context"] = "revi/review"
	req := ProvisionRequest{
		TenantID:     "t1",
		ConnectionID: conn.ID,
		RepoFullName: "group/api",
		BotIDs:       []string{"review-pr"},
		LaunchVars:   echo,
		ActorID:      "tester",
		Replace:      true,
	}
	_, err = o.Provision(context.Background(), req)
	if !errors.Is(err, ErrProvisionDiverged) {
		t.Fatalf("mid-write failure: err = %v, want ErrProvisionDiverged — a partial write must fail loudly, not return success", err)
	}
	// The enforcement half was written FIRST: the repo already applies the new
	// context even though the report (the integration) still shows the old.
	cfg, cerr := o.Webhooks.Get(context.Background(), res.WebhookID)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if got := cfg.OperatorLaunchVars["gate_context"]; got != "revi/review" {
		t.Errorf("enforcement half after the crash: gate_context = %q, want the new value written first", got)
	}
	// The report half was NOT written: re-running the same request converges.
	if _, err := o.Provision(context.Background(), req); err != nil {
		t.Fatalf("re-run did not converge: %v", err)
	}
	integ, cfg = readBothStores(t, o, res)
	for key, want := range echo {
		if got := integ.LaunchVars[key]; got != want {
			t.Errorf("after recovery, integration %s = %q, want %q", key, got, want)
		}
		if got := cfg.OperatorLaunchVars[key]; got != want {
			t.Errorf("after recovery, config %s = %q, want %q", key, got, want)
		}
	}
}
