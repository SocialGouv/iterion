package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// anchorFixture wires a server whose grant registry runs on a clock the test
// drives, so the LAUNCH instant and the TERMINAL instant can be days apart —
// the only condition under which the coupling below is observable at all.
func anchorFixture(t *testing.T, inputs map[string]any) (*Server, *ForgePublishTokenRegistry, *time.Time) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s, _ := newForgePublishTestServer(t)
	s.cfg.Store = st
	clock := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	reg := NewForgePublishTokenRegistry()
	reg.now = func() time.Time { return clock }
	s.forgePublishTokens = reg
	if err := reg.Register("tok-anchor", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), "run-anchor", "review_pr", inputs); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return s, reg, &clock
}

func anchorInputs(gating bool) map[string]any {
	in := map[string]any{
		forgePublishVarToken: "tok-anchor",
		"pr_url":             "https://github.com/o/r/pull/42",
		"head_sha":           "deadbeef",
	}
	if gating {
		in["gate_context"] = "iterion/review"
	}
	return in
}

func terminalise(t *testing.T, s *Server) {
	t.Helper()
	run, err := s.cfg.Store.LoadRun(context.Background(), "run-anchor")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.Status = store.RunStatusFailed
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

// The coupling the branch is built on, measured at the instants that actually
// decide it rather than by comparing two constants.
//
// A grant's expiry is stamped at LAUNCH (Register: launch + forgePublishDefaultTTL).
// The net that reads it is anchored on the run's TERMINAL updated_at (the
// sweep offers a run until updated_at + gateSweepHorizon). Those two are the
// same only for a run that dies promptly — and the runs this whole net exists
// for are the opposite case: a run parked on a weekly provider window reaches
// terminal up to retrypolicy.DefaultMaxWait after it launched.
//
// For that run "terminal + the repair window" falls PAST the launch-stamped
// expiry, so a shorten-only expireIn is a no-op on it and the grant dies with
// days of horizon left to run. Every pass in that gap finds the run, looks up
// its grant, and abstains — a net that from the outside is indistinguishable
// from one still trying. Comparing the two durations cannot see this; only
// walking the clock can.
func TestAGrantOutlivesTheHorizonEvenWhenTheRunParkedForAWeekFirst(t *testing.T) {
	s, reg, clock := anchorFixture(t, anchorInputs(true))

	// The run parks on a usage window for as long as the retry policy allows,
	// then dies without publishing.
	*clock = clock.Add(retrypolicy.DefaultMaxWait)
	terminal := *clock
	terminalise(t, s)
	if err := s.expireForgePublishGrantForRun(context.Background(), "run-anchor"); err != nil {
		t.Fatalf("expire: %v", err)
	}

	// The last instant the sweep will still offer this run to the reconciler.
	*clock = terminal.Add(gateSweepHorizon)
	if _, ok := reg.lookup("tok-anchor"); !ok {
		t.Fatalf("the grant is dead %s after the run ended, but the sweep offers it until %s after — every pass in between can only abstain, which reads exactly like a net still trying",
			time.Duration(0), gateSweepHorizon)
	}

	// And it does not outlive its purpose: past the horizon nothing revisits
	// the run, so the grant must not keep a forge-write credential alive.
	*clock = terminal.Add(forgePublishPostRunGrace).Add(time.Minute)
	if _, ok := reg.lookup("tok-anchor"); ok {
		t.Error("the grant outlived the horizon that is its only remaining reader — re-anchoring must bound the life, not uncap it")
	}
}

// The other direction of the same predicate, and the reason the long life
// above is affordable at all.
//
// The server mints a publish grant for ANY bot launched with a pr_url — the
// brancher, the docs amender, the implementer. Only the ones that also claimed
// the repo's gate context owe a required check anything, and only those have a
// reader for their grant after death. Giving the rest the horizon would keep a
// crashed run's forge-WRITE token usable for days, and would stop terminal
// eviction from trimming the registry at all — the invariant forgePublishMaxTokens
// is sized against, and whose failure mode on the in-memory backend is a
// REFUSED launch, not a slow leak.
func TestADeadRunThatClaimedNoGateIsRetiredAtOnce(t *testing.T) {
	s, reg, clock := anchorFixture(t, anchorInputs(false))
	terminal := *clock
	terminalise(t, s)
	if err := s.expireForgePublishGrantForRun(context.Background(), "run-anchor"); err != nil {
		t.Fatalf("expire: %v", err)
	}

	*clock = terminal.Add(forgePublishDeadRunGrace).Add(time.Minute)
	if _, ok := reg.lookup("tok-anchor"); ok {
		t.Errorf("a dead run that claims no gate kept its forge-write grant past %s — nothing reads it, and the registry's cap is sized on this eviction happening",
			forgePublishDeadRunGrace)
	}
}

// A gate claim pinned OFF is not a claim: no verdict will ever replace it, so
// the run owes nothing and its grant has no reader either. Same tier as a run
// with no gate context at all — the readers agree (runGateDisabled is checked
// by the launch claim, the reconciler and the auto-fix lane alike).
func TestAGateDisabledRunIsRetiredLikeAnyOtherDeadRun(t *testing.T) {
	in := anchorInputs(true)
	in["gate_enabled"] = "false"
	s, reg, clock := anchorFixture(t, in)
	terminal := *clock
	terminalise(t, s)
	if err := s.expireForgePublishGrantForRun(context.Background(), "run-anchor"); err != nil {
		t.Fatalf("expire: %v", err)
	}

	*clock = terminal.Add(forgePublishDeadRunGrace).Add(time.Minute)
	if _, ok := reg.lookup("tok-anchor"); ok {
		t.Error("a run whose launch pinned the gate off kept the horizon-long grant — it owes no verdict, so nothing will ever read it")
	}
}

// reanchorIn is the one primitive here that can EXTEND a grant, so its bound
// is load-bearing: without it a caller could hand a forge-write credential an
// arbitrary life, which is exactly what forgePublishDefaultTTL's own comment
// promises cannot happen.
func TestReanchoringCannotGrantAnUnboundedLife(t *testing.T) {
	clock := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	reg := NewForgePublishTokenRegistry()
	reg.now = func() time.Time { return clock }
	if err := reg.Register("tok", ForgePublishGrant{TeamID: "t", ConnectionID: "c", Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	reg.reanchorIn("tok", 10*365*24*time.Hour)

	clock = clock.Add(forgePublishPostRunGrace).Add(time.Minute)
	if _, ok := reg.lookup("tok"); ok {
		t.Errorf("a grant re-anchored past %s stayed alive — the clamp is what keeps an extending primitive from uncapping the credential", forgePublishPostRunGrace)
	}
}

// The asymmetry the tiering rests on, proved rather than eyeballed against a
// field list: whenever runClaimsGate says no, the reconciler posts NOTHING —
// so retiring that grant early can never silence a verdict somebody was going
// to write. The relation must hold in that direction and may not hold in the
// other (the readers add live checks runClaimsGate deliberately omits), which
// is why a drift here is caught by removing a field rather than by adding one.
func TestRunClaimsGateIsWeakerThanEveryReader(t *testing.T) {
	for _, tc := range []struct {
		name  string
		strip func(map[string]any)
	}{
		{"no grant token", func(in map[string]any) { delete(in, forgePublishVarToken) }},
		{"no pr_url", func(in map[string]any) { delete(in, "pr_url") }},
		{"no gate_context", func(in map[string]any) { delete(in, "gate_context") }},
		{"gate pinned off", func(in map[string]any) { in["gate_enabled"] = "false" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatingInputs()
			tc.strip(in)
			gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
			s, runID := gateReconcileFixture(t, in, gc)

			run, err := s.cfg.Store.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if runClaimsGate(run) {
				t.Fatalf("runClaimsGate said yes without %s — the predicate must be the weakest of the three", tc.name)
			}
			if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if gc.setCalls != 0 {
				t.Errorf("the reconciler posted %d status(es) for a run runClaimsGate rejects — then retiring its grant early silences a verdict somebody was going to write", gc.setCalls)
			}
		})
	}
}

// Re-anchoring must not RESURRECT. lookup treats a grant as gone from its
// expiry instant while the map entry lingers until Register next sweeps, so
// the map is not the authority on whether a grant is alive — and a reaped
// grant coming back would hand a dead run's forge-write token a second life.
func TestReanchoringNeverResurrectsAnExpiredGrant(t *testing.T) {
	clock := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	reg := NewForgePublishTokenRegistry()
	reg.now = func() time.Time { return clock }
	if err := reg.Register("tok", ForgePublishGrant{TeamID: "t", ConnectionID: "c", Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(forgePublishDefaultTTL).Add(time.Minute)
	reg.reanchorIn("tok", forgePublishPostRunGrace)
	if _, ok := reg.lookup("tok"); ok {
		t.Error("an expired grant was re-anchored back to life")
	}
}
