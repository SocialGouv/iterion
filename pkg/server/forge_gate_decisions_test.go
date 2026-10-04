package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// R3-4: the Valkey claim twin was mirror-edited (tie-break), so pin BOTH
// stores to one claim contract: first claim granted, same-ms tie refused,
// same-ID refresh granted, strictly-newer displaces, and a release lets an
// older live predecessor (Prev) stand again.
func TestGateDecisionClaim_ParityAcrossStores(t *testing.T) {
	_, rdb := newTestRedis(t)
	stores := map[string]gateDecisionStore{
		"memory": newMemoryGateDecisionStore(nil),
		"valkey": newValkeyGateDecisionStore(rdb),
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			mk := func(id string, at time.Time, state string) gateMark {
				return gateMark{Decision: gateDecision{ID: id, At: at}, Status: gateMarkStatus{State: state, Description: id}}
			}
			claim := func(m gateMark) bool {
				ok, err := store.claim(ctx, "k", m, gateDecisionTTL)
				if err != nil {
					t.Fatalf("claim %s: %v", m.Decision.ID, err)
				}
				return ok
			}
			if !claim(mk("d1", at, "success")) {
				t.Fatal("first claim must be granted")
			}
			if claim(mk("d2", at, "failure")) {
				t.Fatal("a different decision in the same truncated ms displaced the incumbent")
			}
			if !claim(mk("d1", at, "success")) {
				t.Fatal("the same-ID re-claim refresh must stay granted")
			}
			if !claim(mk("d3", at.Add(time.Millisecond), "failure")) {
				t.Fatal("a strictly newer decision must displace the incumbent")
			}
			// Release d3: the mark stands abandoned, and the key's LIVE
			// decision is its predecessor d1 — which a same-ms twin of d3
			// must not be refused against.
			if err := store.release(ctx, "k", gateDecision{ID: "d3", At: at.Add(time.Millisecond)}); err != nil {
				t.Fatalf("release: %v", err)
			}
			live, found, err := store.newest(ctx, "k")
			if err != nil || !found || live.Decision.ID != "d1" {
				t.Fatalf("after release the live decision must fall back to Prev (d1), got %+v found=%v err=%v", live, found, err)
			}
			if !claim(mk("d4", at.Add(time.Millisecond), "success")) {
				t.Fatal("a decision newer than the live Prev must claim after the release")
			}
		})
	}
}

// R2-LOW-MED (tie): two DIFFERENT decisions inside one truncated millisecond
// must not both win. The incumbent keeps the check — a claim that is not
// STRICTLY newer is refused — or a reconciler's synthetic failure claiming in
// the same millisecond as an operator's approve overwrites the force-green.
// The same-ID re-claim (a replay refreshing its own mark) must stay granted.
//
// Mutation that reddens this test: a claim granted on a timestamp tie
// (refusing only a strictly-newer live mark).
func TestGateDecisionClaim_TieBreaksTowardTheIncumbent(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)

	mk := func(id string, at time.Time) gateMark {
		return gateMark{Decision: gateDecision{ID: id, At: at}, Status: gateMarkStatus{State: "success", Description: id}}
	}
	granted, err := s.gateDecisions.claim(ctx, "k", mk("d1", at), gateDecisionTTL)
	if err != nil || !granted {
		t.Fatalf("the first claim must be granted (granted=%v err=%v)", granted, err)
	}
	granted, err = s.gateDecisions.claim(ctx, "k", mk("d2", at), gateDecisionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if granted {
		t.Fatal("a DIFFERENT decision in the same truncated millisecond displaced the incumbent — the tie must keep the check with whoever claimed first")
	}
	// Same-ID refresh: a replay re-claiming its own mark stays granted.
	granted, err = s.gateDecisions.claim(ctx, "k", mk("d1", at), gateDecisionTTL)
	if err != nil || !granted {
		t.Fatalf("the same-ID re-claim refresh must stay unaffected (granted=%v err=%v)", granted, err)
	}
	// Strictly newer still wins.
	granted, err = s.gateDecisions.claim(ctx, "k", mk("d3", at.Add(time.Millisecond)), gateDecisionTTL)
	if err != nil || !granted {
		t.Fatalf("a strictly newer decision must displace the incumbent (granted=%v err=%v)", granted, err)
	}
}

// S3-HIGH1 probe, pinned at the authority: R2 claims its check with a REAL
// success decision; a refused run's reconciler then claims with its
// retroactive (terminal-instant) anchor — "newer", so its synthetic mark
// demotes R2's — and posts its synthetic failure. R2's success lands on top,
// and R2's OWN reassert must leave it there: re-posting the mark's synthetic
// status buries a legitimate verdict under a marker the reconciler can
// re-derive on any later pass, and nothing ever heals it.
//
// Mutation that reddens this test: deleting the isSyntheticGateInterruption
// guard in reassertNewerVerdict.
func TestGateDecisionReassert_NeverPutsASyntheticBackOverARealVerdict(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	conn, err := s.forgeConnections.Get(context.Background(), "conn1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := gateDecisionKey(conn, "o/r", "deadbeef", "iterion/review")

	// R2's real verdict decision, taken at t2.
	realDecision := newGateDecision(time.Now().Add(-time.Minute))
	claimed, err := s.claimGateDecision(ctx, key, gateMark{Decision: realDecision, Status: gateMarkStatus{
		State: string(forge.CommitStateSuccess), Description: "no blocking findings (≥high); 0 total",
	}})
	if err != nil || !claimed {
		t.Fatalf("R2's claim: granted=%v err=%v", claimed, err)
	}
	// The reconciler's synthetic decision, anchored at the run's death — a
	// minute LATER, so it wins the mark.
	syntheticDecision := newGateDecision(time.Now())
	claimed, err = s.claimGateDecision(ctx, key, gateMark{Decision: syntheticDecision, Status: gateMarkStatus{
		State:       string(forge.CommitStateFailure),
		Description: gateRefusalDescription(&gateRefusal{Reason: "it audited deadbee but the head moved to cafebabe"}),
	}})
	if err != nil || !claimed {
		t.Fatalf("reconciler's claim: granted=%v err=%v", claimed, err)
	}

	// R2's success landed; R2's reassert runs. The mark's "newest" is the
	// synthetic — it must NOT be put back over the real verdict.
	s.reassertNewerVerdict(ctx, gc, key, "o/r", "deadbeef", "iterion/review", realDecision)
	for _, st := range gc.posted {
		if st.State == forge.CommitStateFailure {
			t.Fatalf("reassert re-posted a synthetic %q over a real success that had just landed — the verdict is buried and nothing heals it", st.Description)
		}
	}

	// The contrapositive stays true: a newer REAL verdict IS put back on top.
	newerReal := newGateDecision(time.Now().Add(time.Minute))
	if _, err := s.gateDecisions.claim(ctx, key, gateMark{Decision: newerReal, Status: gateMarkStatus{
		State: string(forge.CommitStateFailure), Description: "2 blocking finding(s) ≥high",
	}}, gateDecisionTTL); err != nil {
		t.Fatal(err)
	}
	s.reassertNewerVerdict(ctx, gc, key, "o/r", "deadbeef", "iterion/review", realDecision)
	if gc.setCalls != 1 || gc.last.Description != "2 blocking finding(s) ≥high" {
		t.Fatalf("a newer real verdict was not re-asserted (calls=%d last=%q) — the guard must exempt synthetics only", gc.setCalls, gc.last.Description)
	}
}
