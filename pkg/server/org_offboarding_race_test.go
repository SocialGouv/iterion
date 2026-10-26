package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// An org membership is the identity boundary a team grant sits inside, so a
// grant that outlives it is live access to a team of an org the holder was
// removed from. Placing a member and off-boarding them are two handlers with
// no shared transaction, which leaves two windows — one per order — and a
// test that reddens in only one of them would be a flake, not a witness.
// Each test below imposes its order through a store wrapper that PAUSES a
// named call, so both interleavings are deterministic.

// racerID is the user both handlers act on; the gates below key on it so the
// caller's own authorization reads (the org admin's membership) pass through.
const racerID = "racer"

// seedRacer registers the target account and its org membership in o1.
func seedRacer(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: racerID, Email: "racer@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: racerID, OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// placementReadGate blocks the FIRST read of the target's org membership —
// the placement handler's pre-check — until the test releases it. The value
// it returns is the one read BEFORE the pause: the stale snapshot the
// handler then acts on.
type placementReadGate struct {
	identity.Store
	userID, orgID string

	mu     sync.Mutex
	paused bool

	reached chan struct{}
	release chan struct{}
}

func newPlacementReadGate(st identity.Store, userID, orgID string) *placementReadGate {
	return &placementReadGate{
		Store: st, userID: userID, orgID: orgID,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
}

func (g *placementReadGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	om, err := g.Store.GetOrgMembership(ctx, userID, orgID)
	if userID != g.userID || orgID != g.orgID {
		return om, err
	}
	g.mu.Lock()
	first := !g.paused
	g.paused = true
	g.mu.Unlock()
	if !first {
		return om, err
	}
	close(g.reached)
	<-g.release
	return om, err
}

// offboardingDeleteGate blocks the off-boarding handler immediately before it
// removes the org membership — after its grant sweep — so a placement runs
// entirely inside that window.
type offboardingDeleteGate struct {
	identity.Store
	userID, orgID string

	mu     sync.Mutex
	paused bool

	reached chan struct{}
	release chan struct{}
}

func newOffboardingDeleteGate(st identity.Store, userID, orgID string) *offboardingDeleteGate {
	return &offboardingDeleteGate{
		Store: st, userID: userID, orgID: orgID,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
}

func (g *offboardingDeleteGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.DeleteOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	first := !g.paused
	g.paused = true
	g.mu.Unlock()
	if first {
		close(g.reached)
		<-g.release
	}
	return g.Store.DeleteOrgMembership(ctx, userID, orgID)
}

// awaitGate waits for a gated handler to reach the store call this test
// pauses it on. With no deadline a handler that returns EARLY — a future
// change to canManageTeam, GetTeam, GetUser or the role check answering
// 403/404/422 — blocks forever and go test kills the whole package at its
// timeout, losing every other result to a goroutine dump instead of one
// named failure.
func awaitGate(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	awaitGateWithin(t, ch, what, 5*time.Second)
}

// awaitGateWithin is awaitGate with the deadline named, for a scenario whose
// handler deliberately spends a store budget of its own
// (grantCompensationTimeout) before it can return.
func awaitGateWithin(t *testing.T, ch <-chan struct{}, what string, d time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%s never happened — the window this test imposes no longer exists", what)
	}
}

// countingAudit counts the audit inserts that have LANDED. Audit writes are
// detached goroutines (auditWrite → goSafe), so a NEGATIVE assertion — "the
// cascade wrote nothing into that trail" — sampled right after the handler
// returns is a coin flip: the row it looks for may not have been written
// yet.
type countingAudit struct {
	audit.Store
	n atomic.Int64
}

func (c *countingAudit) Insert(ctx context.Context, e audit.Event) error {
	err := c.Store.Insert(ctx, e)
	c.n.Add(1)
	return err
}

// auditQuietWindow is how long awaitAuditQuiet waits for a row nothing
// asked for once the expected ones have landed. Every insert the handler
// spawns is already a runnable goroutine when it answers, so the window
// covers scheduling, not work.
const auditQuietWindow = 200 * time.Millisecond

// awaitAuditQuiet waits for the want inserts a scenario accounts for, then
// for a quiet window, and reports any extra: a barrier, where sampling the
// store once is a measurement of the scheduler. The caller can then assert
// on what a trail does NOT hold.
func awaitAuditQuiet(t *testing.T, c *countingAudit, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.n.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of the %d audit inserts this cascade accounts for landed", c.n.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(auditQuietWindow)
	if got := c.n.Load(); got != want {
		t.Errorf("%d audit rows landed where this cascade accounts for %d — one of them is a row nothing asked for", got, want)
	}
}

// auditRows polls the tenant's audit rows — the writes are detached — and
// returns them keyed by action, giving up at the deadline. The caller asserts
// on the SET of keys, never on an order: two detached inserts land in either
// one. The page asks for the store's own ceiling: the rows wanted are
// selected by TARGET, which no Page filter carries, and an unset Limit would
// serve the 50 newest rows of the whole tenant (audit.ClampLimit).
func auditRows(t *testing.T, s *Server, tenantID, targetID string, want ...string) map[string]audit.Event {
	t.Helper()
	var got map[string]audit.Event
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, err := s.auditStore.ListByTenant(context.Background(), tenantID, audit.Page{Limit: 500})
		if err != nil {
			t.Fatalf("list audit rows of %s: %v", tenantID, err)
		}
		got = map[string]audit.Event{}
		for _, e := range evs {
			if e.TargetID == targetID {
				got[e.Action] = e
			}
		}
		missing := false
		for _, a := range want {
			if _, ok := got[a]; !ok {
				missing = true
			}
		}
		if !missing || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Order 1: the placement reads the org membership, the whole off-boarding
// runs, and only then does the placement write its grant. The pre-check
// cannot see the removal; what catches it is the re-read AFTER the write.
func TestOrgOffboarding_placementReadingBeforeTheRemovalLeavesNoGrant(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	gate := newPlacementReadGate(real, racerID, "o1")
	s.authSvc.SetStoreForTest(gate)

	placed := newErrorRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handlePutTeamMember(placed, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	}()
	awaitGate(t, gate.reached, "the handler reaching the gated store call")

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusNoContent {
		close(gate.release)
		awaitGate(t, done, "the gated handler returning")
		t.Fatalf("off-boarding: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	close(gate.release)
	awaitGate(t, done, "the gated handler returning")

	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatal("the org membership survived the off-boarding")
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Fatalf("team grant %+v outlived the org membership — the placement wrote it against a membership the removal had already taken away", mb)
	}
	if placed.Code != http.StatusConflict {
		t.Fatalf("placement: code=%d body=%s, want 409 — a placement whose org membership vanished must refuse, not report success", placed.Code, placed.Body.String())
	}
	body := placed.Body.String()
	for _, want := range []string{"racer@example.com", "removed from this team's organization",
		"while the placement was in flight", "add them to the organization again"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not name what happened (missing %q): %s", want, body)
		}
	}

	rows := auditRows(t, s, "t1", racerID, "member.removed")
	revoke, ok := rows["member.removed"]
	if !ok {
		t.Errorf("the compensation revoked the grant without an audit row: %v", auditActionsOf(rows))
	} else if revoke.Meta["reason"] != "grant_unwound_org_member_removed" {
		// The COMPENSATION took this grant back — a write this caller
		// issued and had refused — and the membership was definitively
		// GONE (ErrNotFound), not merely unreadable. The cascade's own
		// revoke writes org_member_removed; one code for both would leave
		// an operator reading the trail unable to tell a refused placement
		// from an off-boarding, and neither from an infra incident.
		t.Errorf("the revoke row does not name the path that took the grant: meta=%v", revoke.Meta)
	}
	if _, added := rows["member.added"]; added {
		t.Errorf("a refused placement reported member.added — the audit trail claims a grant that was revoked: %v", auditActionsOf(rows))
	}
}

// auditActionsOf names the actions a row map carries, for assertion messages.
func auditActionsOf(rows map[string]audit.Event) []string {
	out := make([]string, 0, len(rows))
	for k := range rows {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Order 2: the off-boarding sweeps the grants, the whole placement runs
// against a membership that is still there, and only then is the membership
// removed. The placement's own re-read cannot see a removal that has not
// happened yet; what catches it is the off-boarding's second sweep.
func TestOrgOffboarding_placementLandingAfterTheSweepLeavesNoGrant(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	gate := newOffboardingDeleteGate(real, racerID, "o1")
	s.authSvc.SetStoreForTest(gate)

	off := newErrorRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	}()
	awaitGate(t, gate.reached, "the handler reaching the gated store call")

	placed := newErrorRecorder()
	s.handlePutTeamMember(placed, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	if placed.Code != http.StatusOK {
		close(gate.release)
		awaitGate(t, done, "the gated handler returning")
		t.Fatalf("placement: code=%d body=%s, want 200 — the org membership is live at both of its reads", placed.Code, placed.Body.String())
	}
	close(gate.release)
	awaitGate(t, done, "the gated handler returning")

	if off.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatal("the org membership survived the off-boarding")
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Fatalf("team grant %+v outlived the org membership — it landed after the off-boarding's sweep and nothing swept again", mb)
	}

	rows := auditRows(t, s, "t1", racerID, "member.added", "member.removed")
	revoke, revoked := rows["member.removed"]
	if _, added := rows["member.added"]; !added || !revoked {
		t.Errorf("the off-boarding revoked a grant the placement had reported: audit actions = %v, want both member.added and member.removed", auditActionsOf(rows))
	} else if revoke.Meta["reason"] != "org_member_removed" {
		// The CASCADE took this one, not the compensation (the placement's
		// re-read saw a membership that was still live and answered 200).
		// The other order's row carries grant_unwound_org_member_removed:
		// the pair is what makes either code worth reading.
		t.Errorf("the revoke row does not name the path that took the grant: meta=%v", revoke.Meta)
	}
}

// The second pass re-reads the org's TEAMS as well as the user's grants, so a
// team created inside the window — what CreateTeamFor does when an org member
// makes a team, modelled here by its two store writes — is swept with the
// rest. Without the re-read the new team is invisible to a list taken before
// it existed.
func TestOrgOffboarding_aTeamCreatedInsideTheWindowIsSweptToo(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	gate := newOffboardingDeleteGate(real, racerID, "o1")
	s.authSvc.SetStoreForTest(gate)

	off := newErrorRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	}()
	awaitGate(t, gate.reached, "the handler reaching the gated store call")

	if _, err := real.CreateTeam(ctx, identity.Team{
		ID: "t-fresh", Name: "fresh", Slug: "fresh", OrgID: "o1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, teamID := range []string{"t1", "t-fresh"} {
		if err := real.UpsertMembership(ctx, identity.Membership{
			UserID: racerID, TeamID: teamID, Role: identity.RoleOwner, JoinedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(gate.release)
	awaitGate(t, done, "the gated handler returning")

	if off.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	for _, teamID := range []string{"t1", "t-fresh"} {
		if mb, err := real.GetMembership(ctx, racerID, teamID); err == nil {
			t.Errorf("team grant %+v on %s outlived the org membership", mb, teamID)
		}
	}
}

// The cascade's own scope, with nothing racing: an off-boarding revokes every
// grant the user holds on a team OF THIS ORG and leaves the rest alone. Both
// passes run on this path, which is what makes an idempotent DeleteMembership
// load-bearing — the second finds the grants already gone.
func TestOrgOffboarding_theCascadeRevokesTheOrgsGrantsAndNoneOutside(t *testing.T) {
	s := newOrgCredsTestServer(t)
	counted := &countingAudit{Store: audit.NewMemoryStore()}
	s.auditStore = counted
	seedRacer(t, s)
	ctx := context.Background()

	// o2 is a second org the same account belongs to; t9 is its team.
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: racerID, OrgID: "o2", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, teamID := range []string{"t1", "t2", "t9"} {
		if err := s.authStore().UpsertMembership(ctx, identity.Membership{
			UserID: racerID, TeamID: teamID, Role: identity.RoleMember, JoinedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	for _, teamID := range []string{"t1", "t2"} {
		if mb, err := s.authStore().GetMembership(ctx, racerID, teamID); err == nil {
			t.Errorf("team grant %+v on %s survived the off-boarding of its org", mb, teamID)
		}
	}
	if _, err := s.authStore().GetMembership(ctx, racerID, "t9"); err != nil {
		t.Errorf("the grant on t9 was revoked with o1's — an off-boarding from one org must not touch another: %v", err)
	}
	if _, err := s.authStore().GetOrgMembership(ctx, racerID, "o2"); err != nil {
		t.Errorf("the o2 org membership went with o1's: %v", err)
	}
	for _, teamID := range []string{"t1", "t2"} {
		rows := auditRows(t, s, teamID, racerID, "member.removed")
		if _, ok := rows["member.removed"]; !ok {
			t.Errorf("the cascade revoked the %s grant with nothing in that team's trail: %v", teamID, auditActionsOf(rows))
		}
	}
	// This cascade accounts for exactly three rows: t1's and t2's revokes
	// and o1's org_member.removed. A row in another org's trail is a
	// cross-tenant leak into an audit view o1 cannot see, so the barrier
	// comes before the negative assertion — sampling the store once instead
	// measures the scheduler.
	awaitAuditQuiet(t, counted, 3)
	if rows := auditRows(t, s, "t9", racerID); len(rows) != 0 {
		t.Errorf("the cascade wrote into t9's trail: %v", auditActionsOf(rows))
	}
}

// sweepErrorGate fails the user's grant listing on ONE chosen call — the
// first pass or the second — and nowhere else, so a removal re-issued after
// the failure runs clean. failDeleteOnCall fails the nth grant DELETE
// instead, which is the partial failure a listing error cannot produce: the
// pass stops with the grants before it already revoked. grantInWindow lands
// a grant right after the membership row is gone, the way a placement that
// read the live membership does, which is the grant an unfinished second
// pass leaves behind.
type sweepErrorGate struct {
	identity.Store
	userID           string
	failOnCall       int
	failDeleteOnCall int
	grantInWindow    string

	mu      sync.Mutex
	lists   int
	deletes int
	granted bool
}

func (g *sweepErrorGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	if userID != g.userID {
		return g.Store.ListMembershipsByUser(ctx, userID)
	}
	g.mu.Lock()
	g.lists++
	n := g.lists
	g.mu.Unlock()
	if n == g.failOnCall {
		return nil, errors.New("identity: list memberships by user: connection reset by peer")
	}
	return g.Store.ListMembershipsByUser(ctx, userID)
}

func (g *sweepErrorGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	if userID != g.userID || g.failDeleteOnCall == 0 {
		return g.Store.DeleteMembership(ctx, userID, teamID)
	}
	g.mu.Lock()
	g.deletes++
	n := g.deletes
	g.mu.Unlock()
	if n == g.failDeleteOnCall {
		return errors.New("identity: delete membership: connection reset by peer")
	}
	return g.Store.DeleteMembership(ctx, userID, teamID)
}

func (g *sweepErrorGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if err := g.Store.DeleteOrgMembership(ctx, userID, orgID); err != nil {
		return err
	}
	if g.grantInWindow == "" || userID != g.userID {
		return nil
	}
	g.mu.Lock()
	first := !g.granted
	g.granted = true
	g.mu.Unlock()
	if !first {
		return nil
	}
	return g.UpsertMembership(context.Background(), identity.Membership{
		UserID: g.userID, TeamID: g.grantInWindow, Role: identity.RoleMember, JoinedAt: time.Now(),
	})
}

// A first pass that fails on the LISTING touches nothing: the member keeps
// the org membership and the grants, and the admin retries. A failure
// mid-sweep leaves the partial state its sibling below asserts, and the two
// passes answer the same 500 for opposite states, so each needs its own
// scenario.
func TestOrgOffboarding_aFailedFirstSweepLeavesTheMemberIntact(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.authSvc.SetStoreForTest(&sweepErrorGate{Store: real, userID: racerID, failOnCall: 1})

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusInternalServerError {
		t.Fatalf("off-boarding whose first sweep failed: code=%d body=%s, want 500", off.Code, off.Body.String())
	}
	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
		t.Errorf("the org membership was removed although the sweep before it failed: %v", err)
	}
	if _, err := real.GetMembership(ctx, racerID, "t1"); err != nil {
		t.Errorf("the team grant went while the membership stayed — the failure must abort before anything is touched: %v", err)
	}
}

// A first pass that fails MID-sweep has already revoked the grants it got
// through: each delete is its own store call, with no transaction behind
// them. The member keeps the org membership, so they are partially
// off-boarded — a state the 500 has to describe, because the admin's next
// move is to re-issue the removal, not to assume nothing happened.
func TestOrgOffboarding_aFirstSweepThatFailsMidWayNamesWhatItAlreadyRevoked(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	// Both stores list a user's grants sorted by JoinedAt, so t1 is the one
	// the sweep gets through and t2 the one it fails on — an order the test
	// imposes rather than reads off a map.
	for i, teamID := range []string{"t1", "t2"} {
		if err := real.UpsertMembership(ctx, identity.Membership{
			UserID: racerID, TeamID: teamID, Role: identity.RoleMember,
			JoinedAt: time.Now().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The invitations go before the grants in each pass, so this failure
	// leaves one already revoked — the half of "stay revoked" the grants
	// cannot witness.
	_, invID := invitationFor(t, s, "t1", "racer@example.com", identity.RoleMember)
	s.authSvc.SetStoreForTest(&sweepErrorGate{Store: real, userID: racerID, failDeleteOnCall: 2})

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusInternalServerError {
		t.Fatalf("off-boarding whose first sweep failed mid-way: code=%d body=%s, want 500", off.Code, off.Diagnostic(t))
	}
	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
		t.Errorf("the org membership was removed although the sweep before it failed: %v", err)
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("the pass reported a failure having revoked nothing (%+v stands) — then the state its error describes cannot happen", mb)
	}
	if _, err := real.GetMembership(ctx, racerID, "t2"); err != nil {
		t.Errorf("the grant the sweep failed on went anyway: %v", err)
	}
	if inv, err := real.GetInvitation(ctx, invID); err == nil {
		t.Errorf("the pending invitation the pass revoked before failing came back: %+v", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("reading the revoked invitation: %v, want ErrNotFound", err)
	}
	body := off.Diagnostic(t)
	for _, want := range []string{"org membership stands", "stay revoked", "audit trail", "re-issue the removal"} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 hides the partial off-boarding it left (missing %q): %s", want, body)
		}
	}
	// Where the error says to look: the grant that did go is named in its
	// own team's trail, under the cascade's reason.
	rows := auditRows(t, s, "t1", racerID, "member.removed")
	if revoke, ok := rows["member.removed"]; !ok {
		t.Errorf("the pass revoked the t1 grant with nothing in that team's trail: %v", auditActionsOf(rows))
	} else if revoke.Meta["reason"] != "org_member_removed" {
		t.Errorf("the revoke row does not name the cascade that took the grant: meta=%v", revoke.Meta)
	}
	// And the advice works: the re-issued removal finishes the sweep.
	again := newErrorRecorder()
	s.handleRemoveOrgMember(again, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if again.Code != http.StatusNoContent {
		t.Fatalf("re-issued removal: code=%d body=%s, want 204 — the error told the admin to re-issue it", again.Code, again.Diagnostic(t))
	}
	if mb, err := real.GetMembership(ctx, racerID, "t2"); err == nil {
		t.Errorf("team grant %+v survived the re-issued removal", mb)
	}
}

func TestOrgOffboarding_aFailedSecondSweepNamesWhatMayBeLeft(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	log := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelInfo, log)
	s.authSvc.SetStoreForTest(&sweepErrorGate{Store: real, userID: racerID, failOnCall: 2, grantInWindow: "t1"})

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusInternalServerError {
		t.Fatalf("off-boarding whose second sweep failed: code=%d body=%s, want 500", off.Code, off.Diagnostic(t))
	}
	body := off.Diagnostic(t)
	for _, want := range []string{
		"may still hold a team grant",
		"re-issue the removal",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the error hides what the unfinished sweep may have left, or what to do (missing %q): %s", want, body)
		}
	}
	// And the grants ALONE: the last sweep runs after this failure and went
	// through here, so an invitation residue in this body would send an
	// operator after a row that sweep took. Its own failure has a clause of
	// its own (lastSweepResidue).
	if strings.Contains(body, standingMintedInvitation) {
		t.Errorf("the 500 of a failed grant pass names an invitation residue the last sweep took: %s", body)
	}
	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Error("the org membership stands despite the 500 — the response must describe what actually happened")
	}
	// Said where it survives the caller too, and with both halves: the
	// detached pass's failure reaches a client that may be gone.
	if got := log.String(); !strings.Contains(got, "the second sweep of the team access of "+racerID) && !strings.Contains(got, racerID) {
		t.Errorf("the log of the failed second sweep does not name the member it was about: %s", got)
	}
	// The state the error names, asserted: a grant IS standing.
	if _, err := real.GetMembership(ctx, racerID, "t1"); err != nil {
		t.Errorf("the grant the error says may be left is gone: %v", err)
	}
	// And the advice it gives works: the removal is idempotent, so the
	// re-issued call finishes the sweep instead of answering 404.
	again := newErrorRecorder()
	s.handleRemoveOrgMember(again, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if again.Code != http.StatusNoContent {
		t.Fatalf("re-issued removal: code=%d body=%s, want 204 — the error told the admin to re-issue it", again.Code, again.Diagnostic(t))
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v survived the re-issued removal", mb)
	}
}

// recheckErrorGate serves the placement's pre-check from the store and then
// fails its re-read — a store blip, not a removal — and optionally fails the
// revoke that follows. The second guard of the compensation and the third
// (the revoke that itself fails) each get their own scenario: two guards that
// cover each other are two guards nothing tests.
type recheckErrorGate struct {
	identity.Store
	userID, orgID, teamID string
	failRevoke            bool
	// gone makes the re-read answer ErrNotFound — a member who WAS removed
	// — instead of a store blip: the branch that answers 409, and the only
	// one whose status the failed revoke overrides.
	gone bool

	// wrote marks the grant write this scenario is about — the named account
	// in the named team: the read it cannot serve is the compensation's,
	// which FOLLOWS that write. Keyed on the write rather than on a read
	// count — the handler takes reads of the same row before it (the
	// boundary pre-check, the rank guard), and an ordinal moves with them.
	wrote atomic.Bool
}

func (g *recheckErrorGate) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	err := g.Store.UpsertMembership(ctx, mb)
	if mb.UserID == g.userID && mb.TeamID == g.teamID {
		g.wrote.Store(true)
	}
	return err
}

func (g *recheckErrorGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	if g.wrote.Load() {
		if g.gone {
			return identity.OrgMembership{}, identity.ErrNotFound
		}
		return identity.OrgMembership{}, errors.New("identity: get org membership: connection reset by peer")
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

func (g *recheckErrorGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	if g.failRevoke && userID == g.userID {
		return errors.New("identity: delete membership: connection reset by peer")
	}
	return g.Store.DeleteMembership(ctx, userID, teamID)
}

// A re-read that FAILS is not a membership that is gone, and it is not a
// membership that is there either. The grant goes back: an identity boundary
// nothing can re-confirm does not hold a grant up, and the admin retries
// against an idempotent endpoint.
func TestOrgOffboarding_anUnconfirmableMembershipTakesTheGrantBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	s.authSvc.SetStoreForTest(&recheckErrorGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1"})

	w := newErrorRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("placement whose re-read failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Fatalf("team grant %+v stands on an org membership the handler could not re-confirm", mb)
	}
	body := w.Diagnostic(t)
	for _, want := range []string{"re-confirm", "place them in the team again"} {
		if !strings.Contains(body, want) {
			t.Errorf("the error does not say what could not be established, or what to do (missing %q): %s", want, body)
		}
	}
	// And the durable row says which of the two things happened: the
	// compensation unwound its own write, because the store could not
	// answer — not because the member was removed.
	rows := auditRows(t, s, "t1", racerID, "member.removed")
	revoke, ok := rows["member.removed"]
	if !ok {
		t.Errorf("the compensation revoked the grant without an audit row: %v", auditActionsOf(rows))
	} else if revoke.Meta["reason"] != "grant_unwound_org_membership_unconfirmed" {
		t.Errorf("the revoke row does not tell an unreadable boundary from a removed member: meta=%v", revoke.Meta)
	}
}

// And when the revoke itself fails there IS a grant with no confirmed
// membership behind it. The only honest answer names it, so an operator
// reading the 500 knows what to go and look at.
func TestOrgOffboarding_aFailedRevokeNamesTheGrantItCouldNotTakeBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)

	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelError, logs)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&recheckErrorGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1", failRevoke: true})

	w := newErrorRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("placement whose revoke failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if body := w.Diagnostic(t); !strings.Contains(body, "may hold a team grant") {
		t.Errorf("the error hides the grant it could not take back: %s", body)
	}
	// The state the message describes, asserted: the row IS standing. A
	// message is not a witness — a change that revoked the grant anyway
	// would leave the assertion above green while the error lied.
	if mb, err := real.GetMembership(context.Background(), racerID, "t1"); err != nil {
		t.Errorf("the grant the error says may be standing is gone: %v", err)
	} else if mb.TeamID != "t1" {
		t.Errorf("the standing grant is not the one the error names: %+v", mb)
	}
	// And it is said where it survives the caller: the body goes to a client
	// that may already be gone, which is how this grant got left in the
	// first place.
	logged := logs.String()
	for _, want := range []string{racerID, "t1", "o1", "revoking it failed"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log line does not name what stands (missing %q): %s", want, logged)
		}
	}
}

// lostAckRevokeGate is the revoke failure a store really serves: the delete
// APPLIES and then answers an error. DeleteOne cannot tell that from a
// delete that reached nothing, so neither can the handler.
type lostAckRevokeGate struct {
	*recheckErrorGate
}

func (g *lostAckRevokeGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	if userID != g.userID {
		return g.Store.DeleteMembership(ctx, userID, teamID)
	}
	if err := g.Store.DeleteMembership(ctx, userID, teamID); err != nil {
		return err
	}
	return errors.New("identity: delete membership: connection reset by peer")
}

// A revoke that answers an error may still have applied it, so the report of
// a failed revoke says the grant MAY stand and never that it does — the body
// already hedges, and the log beside it is the surface an operator greps for
// the grants to go and take by hand. Here the delete lands and then errors:
// the grant is gone, and a report claiming it stands is false.
func TestOrgOffboarding_aRevokeWhoseAckWasLostIsNotReportedAsAStandingGrant(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelError, logs)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&lostAckRevokeGate{recheckErrorGate: &recheckErrorGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1"}})

	w := newErrorRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	s.authSvc.SetStoreForTest(real)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("placement whose revoke lost its ack: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if mb, err := real.GetMembership(context.Background(), racerID, "t1"); err == nil {
		t.Fatalf("the delete did not apply, so this is not the lost-ack case: %+v", mb)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("reading the grant back: %v, want identity.ErrNotFound", err)
	}
	if body := w.Diagnostic(t); !strings.Contains(body, "may hold a team grant") {
		t.Errorf("the body says more than the store answered: %s", body)
	}
	logged := logs.String()
	if !strings.Contains(logged, "revoking it failed") {
		t.Fatalf("the failed revoke is not logged where it survives the caller: %s", logged)
	}
	if strings.Contains(logged, "stands with no org membership") {
		t.Errorf("the log asserts a standing grant for one that is gone — a failed delete can be a lost ack: %s", logged)
	}
}

// A removed member and a revoke that fails are the same branch twice over:
// the 409 belongs to the grant that WENT back, so when the revoke fails
// there is none to report — a grant is standing, which is a 500 whose body
// names it. A client that read the conflict as "the grant is gone" would
// close the incident on an orphan.
func TestOrgOffboarding_aRemovedMemberWhoseRevokeFailsAnswers500NotTheConflict(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelError, logs)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&recheckErrorGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1", gone: true, failRevoke: true})

	w := newErrorRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("placement on a removed member whose revoke failed: code=%d body=%s, want 500 — the conflict would say the grant went back", w.Code, w.Diagnostic(t))
	}
	body := w.Diagnostic(t)
	for _, want := range []string{"removed from this team's organization", "may hold a team grant"} {
		if !strings.Contains(body, want) {
			t.Errorf("the error hides the removal, or the grant it could not take back (missing %q): %s", want, body)
		}
	}
	if mb, err := real.GetMembership(context.Background(), racerID, "t1"); err != nil {
		t.Errorf("the grant the error says may be standing is gone: %v", err)
	} else if mb.TeamID != "t1" {
		t.Errorf("the standing grant is not the one the error names: %+v", mb)
	}
	if logged := logs.String(); !strings.Contains(logged, "revoking it failed") {
		t.Errorf("the standing grant is not logged where it survives the caller: %s", logged)
	}
}

// slowConfirmGate imposes order 1 — the placement reads the org membership,
// the whole off-boarding runs, the placement then writes its grant — and
// makes the compensation's re-read answer only at its DEADLINE, which is
// what MongoStore does when a stepdown, a primary election or a saturated
// pool blocks the driver in server selection. Its writes are healthy; they
// only refuse a context that is already done, as MongoStore's DeleteOne
// does. An error returned INSTANTLY, as recheckErrorGate's is, is a fixture
// kinder than the store: it hands the revoke a budget the real failure has
// already spent.
type slowConfirmGate struct {
	identity.Store
	userID, orgID, teamID string

	mu    sync.Mutex
	reads int

	// wrote marks the grant write this scenario is about — the named account
	// in the named team: the read it cannot serve is the compensation's,
	// which FOLLOWS that write. Keyed on the write rather than on a read
	// count — the handler takes reads of the same row before it (the
	// boundary pre-check, the rank guard), and an ordinal moves with them.
	// It is held under mu with the read count, so one decision reads one
	// snapshot of both; postWriteReads counts the reads that follow the
	// write, of which exactly the FIRST is the compensation's.
	wrote          bool
	postWriteReads int

	reached chan struct{}
	release chan struct{}

	revokes          atomic.Int64
	revokesOnDoneCtx atomic.Int64
	unboundedRevokes atomic.Int64
	unboundedReReads atomic.Int64
}

func (g *slowConfirmGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	om, err := g.Store.GetOrgMembership(ctx, userID, orgID)
	g.mu.Lock()
	g.reads++
	n := g.reads
	wrote := g.wrote
	if wrote {
		g.postWriteReads++
	}
	postWrite := g.postWriteReads
	g.mu.Unlock()
	// Two reads are this scenario's: the placement's pre-check, which is the
	// FIRST, and the compensation's re-read, which is the first to follow the
	// WRITE. Every other read — the rank guard's, the authorization reads of
	// whatever the test drives next, which is how the roster probe at the end
	// of this scenario reads — is served normally: a gate that slowed them
	// too would block on a request context that has no deadline at all, and
	// would count an ordinary read as this scenario's failure.
	switch {
	case !wrote && n == 1:
		// The placement's pre-check: pause it on the value read while the
		// membership was still live, which is the stale snapshot it acts on.
		close(g.reached)
		<-g.release
		return om, err
	case wrote && postWrite == 1:
		// The compensation's re-read: it cannot answer inside its budget.
		if _, ok := ctx.Deadline(); !ok {
			// Waiting on a context with no deadline would hang until the
			// package timeout and lose every other result to a goroutine
			// dump; the counter turns that into a named failure.
			g.unboundedReReads.Add(1)
			return identity.OrgMembership{}, errors.New("identity: get org membership: the re-read carries no deadline to spend")
		}
		<-ctx.Done()
		return identity.OrgMembership{}, fmt.Errorf("identity: get org membership: %w", ctx.Err())
	}
	return om, err
}

func (g *slowConfirmGate) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	err := g.Store.UpsertMembership(ctx, mb)
	if mb.UserID == g.userID && mb.TeamID == g.teamID {
		g.mu.Lock()
		g.wrote = true
		g.mu.Unlock()
	}
	return err
}

func (g *slowConfirmGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	if userID != g.userID {
		return g.Store.DeleteMembership(ctx, userID, teamID)
	}
	g.revokes.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		// A detached revoke with no deadline waits on the driver's own
		// timeouts during the incident this guard exists for; the counter
		// turns that into a named failure.
		g.unboundedRevokes.Add(1)
	}
	if err := ctx.Err(); err != nil {
		g.revokesOnDoneCtx.Add(1)
		return fmt.Errorf("identity: delete membership: %w", err)
	}
	return g.Store.DeleteMembership(ctx, userID, teamID)
}

// Order 1 with the store failing where it really fails: at the deadline, not
// instantly. The re-read and the revoke therefore cannot share one budget —
// a re-read that spends it leaves exactly the grant this guard exists to
// remove, and canViewTeam authorizes on that grant alone, so the account the
// org removed goes on reading the team roster through it. The test costs one
// grantCompensationTimeout of wall clock: that budget being spent IS the
// scenario, and an instant error cannot witness it.
func TestOrgOffboarding_aReReadThatAnswersOnlyAtItsDeadlineStillTakesTheGrantBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelError, logs)
	ctx := context.Background()

	real := s.authStore()
	gate := &slowConfirmGate{
		Store: real, userID: racerID, orgID: "o1", teamID: "t1",
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	s.authSvc.SetStoreForTest(gate)

	placed := newErrorRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handlePutTeamMember(placed, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	}()
	awaitGate(t, gate.reached, "the placement reaching its pre-check")

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusNoContent {
		close(gate.release)
		awaitGateWithin(t, done, "the gated handler returning", 4*grantCompensationTimeout)
		t.Fatalf("off-boarding: code=%d body=%s, want 204", off.Code, off.Diagnostic(t))
	}
	close(gate.release)
	awaitGateWithin(t, done, "the placement returning", 4*grantCompensationTimeout)

	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatal("the org membership survived the off-boarding")
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v outlived the org membership: the revoke ran on the budget the re-read had already spent", mb)
	}
	if n := gate.revokesOnDoneCtx.Load(); n != 0 {
		t.Errorf("%d of the %d revokes ran on a context that was already done", n, gate.revokes.Load())
	}
	if n := gate.unboundedReReads.Load(); n != 0 {
		t.Errorf("the compensation's re-read ran %d times with no deadline — this scenario cannot witness a spent budget", n)
	}
	if n := gate.unboundedRevokes.Load(); n != 0 {
		t.Errorf("%d of the %d revokes ran with no deadline — grantCompensationTimeout bounds EACH store call of this compensation, and a detached call with no bound outlives the incident",
			n, gate.revokes.Load())
	}
	if placed.Code != http.StatusInternalServerError {
		t.Errorf("placement whose re-read timed out: code=%d body=%s, want 500 — a boundary nothing could certify does not hold a grant up", placed.Code, placed.Diagnostic(t))
	}
	body := placed.Diagnostic(t)
	for _, want := range []string{
		"could not re-confirm", "the team grant was revoked",
		// The grant is taken back with the org membership row UNREADABLE,
		// so the rule that takes the rows kept on it cannot decide and no
		// sweep runs: the body names the row that may stand, the listing
		// this caller can read and the act that takes it, as the sibling
		// exits of the same rule do (sweepInvitationsKeptOnTeamGrant).
		"may stand on it",
		"could not read",
		"GET /api/teams/t1/invitations",
		"revoked by hand",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer does not say the grant went back (missing %q): %s", want, body)
		}
	}
	// The public path the grant would have opened, driven as the holder.
	roster := newErrorRecorder()
	s.handleListTeamMembers(roster, teamReq(auth.WithIdentity(ctx, auth.Identity{UserID: racerID}),
		"GET", "/api/teams/t1/members", "", "t1", ""))
	if roster.Code == http.StatusOK {
		t.Errorf("the off-boarded account reads the t1 roster through a grant the compensation could not take back: %s", roster.Diagnostic(t))
	}
	// The revoke HELD here, so the log the compensation keeps for the grant
	// it could not take back stays empty — the sibling scenario above owns
	// the other half, and a revoke that failed silently would land here.
	if logged := logs.String(); logged != "" {
		t.Errorf("the compensation logged although the revoke held: %s", logged)
	}
}

// contextHonouringGate is the one behaviour MemoryStore cannot model: a
// store that fails the call when the context is already done, as MongoStore
// does (FindOne / DeleteOne / Find fail fast on a cancelled context). It
// cancels the request the moment the target's grant is written — a client
// that goes away right after the write (an abort, a CLI ^C, a proxy read
// timeout), which is also the most ordinary reason a membership cannot be
// re-confirmed. The compensation that answers it must therefore not run on
// the request's own context.
type contextHonouringGate struct {
	identity.Store
	userID, orgID string
	cancel        func()

	mu      sync.Mutex
	paused  bool
	reached chan struct{}
	release chan struct{}
}

func (g *contextHonouringGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	if err := ctx.Err(); err != nil {
		return identity.OrgMembership{}, fmt.Errorf("identity: get org membership: %w", err)
	}
	om, err := g.Store.GetOrgMembership(ctx, userID, orgID)
	g.mu.Lock()
	first := !g.paused
	g.paused = true
	g.mu.Unlock()
	if !first {
		return om, err
	}
	close(g.reached)
	<-g.release
	return om, err
}

func (g *contextHonouringGate) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("identity: upsert membership: %w", err)
	}
	err := g.Store.UpsertMembership(ctx, mb)
	if err == nil && mb.UserID == g.userID {
		g.cancel()
	}
	return err
}

func (g *contextHonouringGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	if userID == g.userID {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("identity: delete membership: %w", err)
		}
	}
	return g.Store.DeleteMembership(ctx, userID, teamID)
}

// A placement whose client is gone still loses the grant. The compensation
// is what the ticket's invariant rests on, so it cannot depend on the
// caller still listening: the dying request context would fail the re-read
// (with a store error, not ErrNotFound) AND the revoke, leaving exactly the
// grant this handler exists not to leave — a 500 written to a socket nobody
// reads being the only trace.
func TestOrgOffboarding_aClientGoneAfterTheGrantWriteStillLosesTheGrant(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	bg := context.Background()

	real := s.authStore()
	ctx, cancel := context.WithCancel(orgAdminCtx())
	defer cancel()
	gate := &contextHonouringGate{
		Store: real, userID: racerID, orgID: "o1", cancel: cancel,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	s.authSvc.SetStoreForTest(gate)

	placed := newErrorRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handlePutTeamMember(placed, teamReq(ctx, "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	}()
	awaitGate(t, gate.reached, "the handler reaching the gated store call")

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusNoContent {
		close(gate.release)
		awaitGate(t, done, "the gated handler returning")
		t.Fatalf("off-boarding: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	close(gate.release)
	awaitGate(t, done, "the gated handler returning")

	if _, err := real.GetOrgMembership(bg, racerID, "o1"); err == nil {
		t.Fatal("the org membership survived the off-boarding")
	}
	if mb, err := real.GetMembership(bg, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v outlived the org membership — the compensation died with the request (placement answered %d: %s)",
			mb, placed.Code, placed.Body.String())
	}
	if placed.Code != http.StatusConflict {
		t.Errorf("placement: code=%d body=%s, want 409 — the membership was gone, which the re-read can still establish on a context of its own",
			placed.Code, placed.Body.String())
	}
}

// offboardingCancelGate honours the context on the calls the second sweep
// makes, lands a grant inside the window the way a placement does, and
// cancels the request right after the membership row is gone.
type offboardingCancelGate struct {
	identity.Store
	userID, orgID, teamID string
	cancel                func()

	mu   sync.Mutex
	done bool
}

func (g *offboardingCancelGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if err := g.Store.DeleteOrgMembership(ctx, userID, orgID); err != nil {
		return err
	}
	if userID != g.userID || orgID != g.orgID {
		return nil
	}
	g.mu.Lock()
	first := !g.done
	g.done = true
	g.mu.Unlock()
	if !first {
		return nil
	}
	if err := g.UpsertMembership(context.Background(), identity.Membership{
		UserID: g.userID, TeamID: g.teamID, Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		return err
	}
	g.cancel()
	return nil
}

func (g *offboardingCancelGate) ListTeamsByOrg(ctx context.Context, orgID string) ([]identity.Team, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("identity: list teams by org: %w", err)
	}
	return g.Store.ListTeamsByOrg(ctx, orgID)
}

func (g *offboardingCancelGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("identity: list memberships by user: %w", err)
	}
	return g.Store.ListMembershipsByUser(ctx, userID)
}

// The same on the off-boarding side: the member row is already gone when
// the second sweep runs, so a client that hangs up in between must not take
// the sweep with it.
func TestOrgOffboarding_aClientGoneAfterTheRemovalStillGetsTheSecondSweep(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	bg := context.Background()

	real := s.authStore()
	ctx, cancel := context.WithCancel(orgAdminCtx())
	defer cancel()
	s.authSvc.SetStoreForTest(&offboardingCancelGate{
		Store: real, userID: racerID, orgID: "o1", teamID: "t1", cancel: cancel,
	})

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(ctx, "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))

	if _, err := real.GetOrgMembership(bg, racerID, "o1"); err == nil {
		t.Fatal("the org membership survived the off-boarding")
	}
	if mb, err := real.GetMembership(bg, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v outlived the org membership — the second sweep died with the request (off-boarding answered %d: %s)",
			mb, off.Code, off.Body.String())
	}
	if off.Code != http.StatusNoContent {
		t.Errorf("off-boarding: code=%d body=%s, want 204 — the sweep it owes runs on a context of its own", off.Code, off.Body.String())
	}
}

// roleChangeReadGate blocks the role change on its read of the grant — the
// value it then writes back is the snapshot taken BEFORE the pause.
type roleChangeReadGate struct {
	identity.Store
	userID, teamID string

	mu     sync.Mutex
	paused bool

	reached chan struct{}
	release chan struct{}
}

func (g *roleChangeReadGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	mb, err := g.Store.GetMembership(ctx, userID, teamID)
	if userID != g.userID || teamID != g.teamID {
		return mb, err
	}
	g.mu.Lock()
	first := !g.paused
	g.paused = true
	g.mu.Unlock()
	if !first {
		return mb, err
	}
	close(g.reached)
	<-g.release
	return mb, err
}

// PATCH /api/teams/{id}/members/{user_id} is the OTHER handler that writes a
// grant row: it reads the grant and writes it back with a new role, which
// re-creates a row an off-boarding running inside that window has swept.
// Both grant writers therefore owe the same two guards, and the window has
// the same two orders. Order 1: the role change reads the grant, the whole
// off-boarding runs, and the boundary is already gone when the handler
// looks — nothing is written at all.
func TestOrgOffboarding_aRoleChangeReadingBeforeTheRemovalWritesNothing(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	gate := &roleChangeReadGate{
		Store: real, userID: racerID, teamID: "t1",
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	s.authSvc.SetStoreForTest(gate)

	patched := newErrorRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleUpdateMember(patched, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/"+racerID, `{"role":"admin"}`, "t1", racerID))
	}()
	awaitGate(t, gate.reached, "the handler reaching the gated store call")

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusNoContent {
		close(gate.release)
		awaitGate(t, done, "the gated handler returning")
		t.Fatalf("off-boarding: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	close(gate.release)
	awaitGate(t, done, "the gated handler returning")

	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatal("the org membership survived the off-boarding")
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v outlived the org membership — the role change wrote back a grant the off-boarding had swept (PATCH answered %d: %s)",
			mb, patched.Code, patched.Body.String())
	}
	if patched.Code != http.StatusUnprocessableEntity {
		t.Errorf("role change: code=%d body=%s, want 422 — the org membership was already gone when the handler read it, so the grant is never written",
			patched.Code, patched.Body.String())
	}

	// What the orphan would be worth: canViewTeam authorizes on the team
	// grant alone, so a grant left standing is live access, not a stale row.
	roster := newErrorRecorder()
	s.handleListTeamMembers(roster, teamReq(auth.WithIdentity(ctx, auth.Identity{UserID: racerID}),
		"GET", "/api/teams/t1/members", "", "t1", ""))
	if roster.Code != http.StatusForbidden {
		t.Errorf("the off-boarded account reads the team roster: code=%d body=%s", roster.Code, roster.Body.String())
	}
}

// A grant that already has no org membership behind it is the drift
// GET /api/admin/users/{id} reports as orphan_grant. A role change refuses
// it and says so — it does not revoke it: the guard takes back the write it
// made itself, and repairing someone else's drift behind the caller's back
// would hide the very row an operator opens that page to find. The fixture's
// `teamadmin` holds exactly that shape: a t1 grant, no membership in o1.
func TestTeamMembers_aRoleChangeOnAnOrphanGrantIsRefusedWithoutRevokingIt(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()

	w := newErrorRecorder()
	s.handleUpdateMember(w, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/teamadmin", `{"role":"member"}`, "t1", "teamadmin"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("role change on an orphan grant: code=%d body=%s, want 422", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "not a member of this team's organization") {
		t.Errorf("the refusal does not name the boundary that is missing: %s", body)
	}
	// This fixture holds a grant with no USER row behind it, which is the
	// other branch of the subject a refusal names: no address to answer at,
	// so the account id — and no refusal for want of one.
	if body := w.Body.String(); !strings.Contains(body, "user teamadmin is not a member") {
		t.Errorf("the refusal names an account whose user row cannot be read as something other than its id: %s", body)
	}
	mb, err := s.authStore().GetMembership(ctx, "teamadmin", "t1")
	if err != nil {
		t.Fatalf("the refused role change revoked a grant it did not write: %v", err)
	}
	if mb.Role != identity.RoleAdmin {
		t.Errorf("the refused role change wrote the new role anyway: %+v", mb)
	}
}

// roleChangeWriteGate blocks the role change immediately before it writes
// the grant back — after its own org-membership check — so an off-boarding
// runs entirely between that check and the write.
type roleChangeWriteGate struct {
	identity.Store
	userID, teamID string

	mu     sync.Mutex
	paused bool

	reached chan struct{}
	release chan struct{}
}

func (g *roleChangeWriteGate) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	if mb.UserID != g.userID || mb.TeamID != g.teamID {
		return g.Store.UpsertMembership(ctx, mb)
	}
	g.mu.Lock()
	first := !g.paused
	g.paused = true
	g.mu.Unlock()
	if first {
		close(g.reached)
		<-g.release
	}
	return g.Store.UpsertMembership(ctx, mb)
}

// Order 2: the role change's own check passes, the whole off-boarding runs,
// and only then does the write-back land — after the second sweep, which is
// what makes the re-read AFTER the write the only thing that can catch it.
// The resurrected row would carry the role the request asked for, so the
// off-boarded account would come back as team admin.
func TestOrgOffboarding_aRoleChangeLandingAfterTheSweepLeavesNoGrant(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	gate := &roleChangeWriteGate{
		Store: real, userID: racerID, teamID: "t1",
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	s.authSvc.SetStoreForTest(gate)

	patched := newErrorRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleUpdateMember(patched, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/"+racerID, `{"role":"admin"}`, "t1", racerID))
	}()
	awaitGate(t, gate.reached, "the handler reaching the gated store call")

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusNoContent {
		close(gate.release)
		awaitGate(t, done, "the gated handler returning")
		t.Fatalf("off-boarding: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	close(gate.release)
	awaitGate(t, done, "the gated handler returning")

	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatal("the org membership survived the off-boarding")
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v outlived the org membership — the role change wrote back a grant the off-boarding had swept (PATCH answered %d: %s)",
			mb, patched.Code, patched.Body.String())
	}
	if patched.Code != http.StatusConflict {
		t.Errorf("role change: code=%d body=%s, want 409 — the grant it wrote was revoked, so the answer cannot be the new role",
			patched.Code, patched.Body.String())
	} else if body := patched.Body.String(); !strings.Contains(body, "while the role change was in flight") {
		t.Errorf("the refusal names the wrong act — the two writers share one guard, so each must say what IT was doing: %s", body)
	}

	// What the orphan would be worth: canViewTeam authorizes on the team
	// grant alone, so a grant left standing is live access, not a stale row.
	roster := newErrorRecorder()
	s.handleListTeamMembers(roster, teamReq(auth.WithIdentity(ctx, auth.Identity{UserID: racerID}),
		"GET", "/api/teams/t1/members", "", "t1", ""))
	if roster.Code != http.StatusForbidden {
		t.Errorf("the off-boarded account reads the team roster: code=%d body=%s", roster.Code, roster.Body.String())
	}
}

// The role change owes the same answer as the placement when the boundary
// becomes unreadable between its check and its write: the row it wrote goes
// back, since a membership nothing can re-confirm does not hold a grant up.
func TestOrgOffboarding_aRoleChangeWithAnUnconfirmableMembershipTakesTheGrantBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.authSvc.SetStoreForTest(&recheckErrorGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1"})

	w := newErrorRecorder()
	s.handleUpdateMember(w, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/"+racerID, `{"role":"admin"}`, "t1", racerID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("role change whose re-read failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v stands on an org membership the handler could not re-confirm", mb)
	}
	if body := w.Diagnostic(t); !strings.Contains(body, "re-confirm") {
		t.Errorf("the error does not say what could not be established: %s", body)
	}
}

// A team with no parent org is outside the two-level tenancy, so there is no
// boundary to read and both guards step aside: a legacy team keeps taking
// placements and role changes. A guard that reads a boundary that does not
// exist would refuse every one of them.
func TestTeamMembers_aTeamWithNoOrgTakesAPlacementAndARoleChange(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	if _, err := s.authStore().CreateTeam(ctx, identity.Team{
		ID: "t-legacy", Name: "legacy", Slug: "legacy", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "legacyowner", TeamID: "t-legacy", Role: identity.RoleOwner, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	owner := auth.WithIdentity(ctx, auth.Identity{UserID: "legacyowner"})

	placed := newErrorRecorder()
	s.handlePutTeamMember(placed, teamReq(owner, "PUT", "/api/teams/t-legacy/members/"+racerID, `{"role":"member"}`, "t-legacy", racerID))
	if placed.Code != http.StatusOK {
		t.Fatalf("placement in a team with no org: code=%d body=%s, want 200", placed.Code, placed.Body.String())
	}

	patched := newErrorRecorder()
	s.handleUpdateMember(patched, teamReq(owner, "PATCH", "/api/teams/t-legacy/members/"+racerID, `{"role":"admin"}`, "t-legacy", racerID))
	if patched.Code != http.StatusOK {
		t.Fatalf("role change in a team with no org: code=%d body=%s, want 200", patched.Code, patched.Body.String())
	}
	mb, err := s.authStore().GetMembership(ctx, racerID, "t-legacy")
	if err != nil {
		t.Fatalf("the grant is gone: %v", err)
	}
	if mb.Role != identity.RoleAdmin {
		t.Errorf("the role change did not land: %+v", mb)
	}
}

// landedWriteGate models the one partial failure MemoryStore cannot: a grant
// write the store APPLIES and then reports as failed. UpsertMembership is a
// single UpdateOne on the real store, so an ack lost to an expiring deadline
// or a primary stepdown leaves the row written and the call in error.
// landed=false is the other half of that uncertainty — the same error over a
// write that never touched the collection — which no caller can tell apart.
//
// It also pauses the writer's pre-check on the live org membership, so the
// whole off-boarding runs between that check and the write: the ticket's
// order 1, with the write now reporting the failure the handler answered
// with instead of compensating.
type landedWriteGate struct {
	identity.Store
	userID, orgID string
	landed        bool

	// A nil release leaves the pre-check alone, for the scenario that races
	// nothing.
	reached chan struct{}
	release chan struct{}

	mu     sync.Mutex
	paused bool

	writes atomic.Int64
}

func newLandedWriteGate(st identity.Store, landed bool) *landedWriteGate {
	return &landedWriteGate{
		Store: st, userID: racerID, orgID: "o1", landed: landed,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
}

func (g *landedWriteGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	om, err := g.Store.GetOrgMembership(ctx, userID, orgID)
	if userID != g.userID || orgID != g.orgID || g.release == nil {
		return om, err
	}
	g.mu.Lock()
	first := !g.paused
	g.paused = true
	g.mu.Unlock()
	if !first {
		return om, err
	}
	// The value read BEFORE the pause is the stale snapshot the handler acts
	// on; every later read — the compensation's, and the authorization reads
	// of whatever the test drives next — is served from the store.
	close(g.reached)
	<-g.release
	return om, err
}

func (g *landedWriteGate) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	if mb.UserID != g.userID {
		return g.Store.UpsertMembership(ctx, mb)
	}
	if g.landed {
		if err := g.Store.UpsertMembership(ctx, mb); err != nil {
			return err
		}
	}
	g.writes.Add(1)
	return errors.New("identity: upsert membership: context deadline exceeded")
}

// raceAgainstOffBoarding runs write (a placement or a role change) in a
// goroutine, waits for gate to pause it on the live org membership, runs the
// whole off-boarding, and returns the paused writer's answer. The release
// comes before the verdict on the off-boarding: a t.Fatalf with the writer
// still parked kills the package on a goroutine dump instead of naming the
// failure.
func raceAgainstOffBoarding(t *testing.T, s *Server, gate *landedWriteGate, write func(w http.ResponseWriter)) *errorRecorder {
	t.Helper()
	rec := newErrorRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		write(rec)
	}()
	awaitGate(t, gate.reached, "the writer pausing on the live org membership")

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	code, body := off.Code, off.Body.String()
	close(gate.release)
	awaitGate(t, done, "the paused writer returning")
	if code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", code, body)
	}
	return rec
}

// Order 1 with the write reporting a failure it may not have had: the store
// applies the grant and answers an error. A handler that stops at that error
// never runs the compensation, so the grant outlives the org membership —
// the ticket's orphan, behind a 500 that names a write which in fact landed.
func TestOrgOffboarding_aGrantWriteThatLandsAndFailsIsStillTakenBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	gate := newLandedWriteGate(real, true)
	s.authSvc.SetStoreForTest(gate)

	placed := raceAgainstOffBoarding(t, s, gate, func(w http.ResponseWriter) {
		s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	})

	if n := gate.writes.Load(); n != 1 {
		t.Fatalf("%d grant writes landed-and-failed, want 1 — this is not the scenario the test describes", n)
	}
	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatal("the org membership survived the off-boarding")
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v outlived the org membership — a write that reports an error still landed, and no compensation ran (placement answered %d: %s)",
			mb, placed.Code, placed.Body.String())
	}
	if placed.Code != http.StatusConflict {
		t.Errorf("placement whose write landed and failed: code=%d body=%s, want 409 — the membership being gone is why the write must not stand, and it is what the caller can act on",
			placed.Code, placed.Body.String())
	}
	if body := placed.Body.String(); !strings.Contains(body, "removed from this team's organization") {
		t.Errorf("the answer names the write error instead of the boundary that went away: %s", body)
	}
	// Why the conflict outranks the write error: the retry a 500 invites
	// cannot work, so the caller would spend a round trip to learn the same
	// thing.
	again := newErrorRecorder()
	s.handlePutTeamMember(again, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	if again.Code != http.StatusUnprocessableEntity {
		t.Errorf("the placement re-issued after the removal: code=%d body=%s, want 422", again.Code, again.Body.String())
	}

	rows := auditRows(t, s, "t1", racerID, "member.removed")
	revoke, ok := rows["member.removed"]
	if !ok {
		t.Errorf("the compensation took a grant back with nothing in the team's trail: %v", auditActionsOf(rows))
	} else if revoke.Meta["reason"] != "grant_unwound_org_member_removed" {
		t.Errorf("the revoke row does not name the path that took the grant: meta=%v", revoke.Meta)
	}
	if _, added := rows["member.added"]; added {
		t.Errorf("a refused placement reported member.added: %v", auditActionsOf(rows))
	}
	// What the orphan would be worth: canViewTeam authorizes on the team
	// grant alone, so a grant left standing is live access.
	roster := newErrorRecorder()
	s.handleListTeamMembers(roster, teamReq(auth.WithIdentity(ctx, auth.Identity{UserID: racerID}),
		"GET", "/api/teams/t1/members", "", "t1", ""))
	if roster.Code != http.StatusForbidden {
		t.Errorf("the off-boarded account reads the t1 roster: code=%d body=%s", roster.Code, roster.Body.String())
	}
}

// The role change writes a grant row too, so a write-back that lands and
// fails resurrects the grant the off-boarding had just swept. Two writers
// sharing one guard are two call sites: a guard run on one of them is a
// guard the other contradicts.
func TestOrgOffboarding_aRoleChangeWriteThatLandsAndFailsIsStillTakenBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	gate := newLandedWriteGate(real, true)
	s.authSvc.SetStoreForTest(gate)

	patched := raceAgainstOffBoarding(t, s, gate, func(w http.ResponseWriter) {
		s.handleUpdateMember(w, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/"+racerID, `{"role":"admin"}`, "t1", racerID))
	})

	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v outlived the org membership at role %s — the write-back landed and no compensation ran (role change answered %d: %s)",
			mb, mb.Role, patched.Code, patched.Body.String())
	}
	if patched.Code != http.StatusConflict {
		t.Errorf("role change whose write landed and failed: code=%d body=%s, want 409", patched.Code, patched.Body.String())
	} else if body := patched.Body.String(); !strings.Contains(body, "while the role change was in flight") {
		t.Errorf("the refusal names the wrong act — the two writers share one guard, so each must say what IT was doing: %s", body)
	}
}

// The other half of the uncertainty: the same error over a write that never
// landed, with the membership gone. The handler cannot tell the two apart,
// so it answers the conflict and records the revoke either way — one
// member.removed row for a grant that may never have existed, where a trail
// that stayed silent would hide the write that did land.
func TestOrgOffboarding_aGrantWriteThatFailedWithoutLandingIsAnsweredAsAConflict(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	gate := newLandedWriteGate(real, false)
	s.authSvc.SetStoreForTest(gate)

	placed := raceAgainstOffBoarding(t, s, gate, func(w http.ResponseWriter) {
		s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	})

	if placed.Code != http.StatusConflict {
		t.Errorf("placement whose write failed without landing: code=%d body=%s, want 409 — the caller cannot tell a lost ack from a refused write, and neither can the handler",
			placed.Code, placed.Body.String())
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("team grant %+v stands although the write never landed", mb)
	}
	rows := auditRows(t, s, "t1", racerID, "member.removed")
	revoke, ok := rows["member.removed"]
	if !ok {
		t.Errorf("the conflict was answered with nothing in the team's trail — the landed case would then be silent too: %v", auditActionsOf(rows))
	} else if revoke.Meta["reason"] != "grant_unwound_org_member_removed" {
		t.Errorf("the revoke row does not name the path that took the grant: meta=%v", revoke.Meta)
	}
}

// A write that lands and fails inside a boundary that HOLDS is not this
// guard's business: the compensation re-reads the membership, finds it, and
// the caller gets the write error. A guard that revoked here would turn
// every lost ack into a lost grant, and record a removal nothing asked for.
func TestTeamMembers_aFailedWriteInsideALiveMembershipRevokesNothing(t *testing.T) {
	s := newOrgCredsTestServer(t)
	counted := &countingAudit{Store: audit.NewMemoryStore()}
	s.auditStore = counted
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.authSvc.SetStoreForTest(&landedWriteGate{Store: real, userID: racerID, orgID: "o1", landed: true})

	placed := newErrorRecorder()
	s.handlePutTeamMember(placed, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"admin"}`, "t1", racerID))
	if placed.Code != http.StatusInternalServerError {
		t.Fatalf("placement whose write failed inside a live membership: code=%d body=%s, want 500", placed.Code, placed.Diagnostic(t))
	}
	if body := placed.Diagnostic(t); !strings.Contains(body, "upsert membership") {
		t.Errorf("the answer does not name the write that failed: %s", body)
	}
	if _, err := real.GetMembership(ctx, racerID, "t1"); err != nil {
		t.Errorf("the compensation revoked a grant whose org membership is live: %v", err)
	}
	// This placement accounts for no audit row at all: the write failed, so
	// there is no member.added, and the boundary held, so there is no
	// revoke. Sampling the store once instead measures the scheduler.
	awaitAuditQuiet(t, counted, 0)
	if rows := auditRows(t, s, "t1", racerID); len(rows) != 0 {
		t.Errorf("a failed write inside a live membership wrote into the trail: %v", auditActionsOf(rows))
	}
}

// storeBlipGate is the shape of a store-wide incident: the SAME failure hits
// the grant write and the org-membership read that follows it. A pool that
// cannot serve an UpdateOne cannot serve the FindOne after it, and a primary
// stepdown fails both for the whole election, so the two calls fail TOGETHER
// far more often than independently — and that conjunction is the input the
// write-error compensation runs on. The membership ROW never moves here:
// this is a blip, not an off-boarding.
type storeBlipGate struct {
	identity.Store
	userID, orgID, teamID string

	// attempted marks the grant write this scenario is about — the named
	// account in the named team. The write is refused rather than applied, so
	// what it marks is the ATTEMPT: the incident starts there, and the reads
	// that follow it are the compensation's.
	attempted atomic.Bool
}

func (g *storeBlipGate) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	if mb.UserID != g.userID || mb.TeamID != g.teamID {
		return g.Store.UpsertMembership(ctx, mb)
	}
	g.attempted.Store(true)
	return errors.New("identity: upsert membership: context deadline exceeded")
}

// The reads BEFORE the write attempt are served — the incident starts with
// that attempt, which is also what tells them apart from the compensation's
// read: the handler's own boundary pre-check and rank guard read the same row
// first.
func (g *storeBlipGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	if g.attempted.Load() {
		return identity.OrgMembership{}, errors.New("identity: get org membership: context deadline exceeded")
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

// A write that FAILED and a boundary that cannot be READ leave the handler
// knowing neither that its grant landed nor that the membership went away.
// Revoking there takes the grant the member ALREADY held — the compensation
// unwinds the write this request made, and this request made none — and it
// answers a role change the caller never asked to be a removal, on the most
// ordinary incident of the set: one store blip failing both calls.
func TestTeamMembers_aRoleChangeCaughtByAStoreBlipKeepsTheGrantItDidNotWrite(t *testing.T) {
	s := newOrgCredsTestServer(t)
	counted := &countingAudit{Store: audit.NewMemoryStore()}
	s.auditStore = counted
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelError, logs)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.authSvc.SetStoreForTest(&storeBlipGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1"})

	w := newErrorRecorder()
	s.handleUpdateMember(w, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/"+racerID, `{"role":"admin"}`, "t1", racerID))

	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
		t.Fatalf("the org membership went — then this is not the scenario the test describes: %v", err)
	}
	mb, err := real.GetMembership(ctx, racerID, "t1")
	if err != nil {
		t.Fatalf("the role change failed AND took back the grant the member already held: %v", err)
	}
	if mb.Role != identity.RoleMember {
		t.Errorf("the write the store reported as failed landed anyway: %+v", mb)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("role change caught by a store blip: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if body := w.Diagnostic(t); !strings.Contains(body, "upsert membership") {
		t.Errorf("the answer does not name the write that failed: %s", body)
	}
	// The grant may have landed unacknowledged while the boundary could not
	// be read: nothing certifies that state, so it is logged where it
	// survives the caller.
	if logged := logs.String(); !strings.Contains(logged, "could not be re-confirmed") {
		t.Errorf("a write that may have landed outside an unreadable boundary is logged nowhere: %q", logged)
	}
	// No grant was written and none was taken back, so this request accounts
	// for no audit row at all.
	awaitAuditQuiet(t, counted, 0)
}

// The same incident on the placement. Two writers share this guard, so a
// guard corrected on one of them is a guard the other contradicts: PUT
// re-roles a grant the member already holds, its write fails, and the
// boundary read fails with it.
func TestTeamMembers_aPlacementCaughtByAStoreBlipKeepsTheGrantItDidNotWrite(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.authSvc.SetStoreForTest(&storeBlipGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1"})

	w := newErrorRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, `{"role":"admin"}`, "t1", racerID))

	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
		t.Fatalf("the org membership went — then this is not the scenario the test describes: %v", err)
	}
	mb, err := real.GetMembership(ctx, racerID, "t1")
	if err != nil {
		t.Fatalf("the placement failed AND took back the grant the member already held: %v", err)
	}
	if mb.Role != identity.RoleMember {
		t.Errorf("the write the store reported as failed landed anyway: %+v", mb)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("placement caught by a store blip: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if body := w.Diagnostic(t); !strings.Contains(body, "upsert membership") {
		t.Errorf("the answer does not name the write that failed: %s", body)
	}
}

// seedConfigEditorGrant builds, through the production path
// (POST /api/teams/{id}/invitations + accept), the state ADR-078 decision 4
// sanctions: a grant carrying the orthogonal config_editor capability with
// NO org membership behind it. identity.OrgRoleForTeamRole answers "" for a
// rank-0 capability, so pkg/auth's grantOrgMembershipForTeam mirrors nothing
// up — an org-member row would pass canViewOrg, which is what the ADR
// forbids.
func seedConfigEditorGrant(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	u, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "cfged", Email: "cfged@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.authSvc.CreateInvitation(ctx, "t1", u.Email, identity.RoleConfigEditor, "teamadmin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, u.ID, tok); err != nil {
		t.Fatal(err)
	}
	mb, err := s.authStore().GetMembership(ctx, u.ID, "t1")
	if err != nil || mb.Role != identity.RoleConfigEditor {
		t.Fatalf("the config_editor grant did not land: %+v (%v)", mb, err)
	}
	if om, err := s.authStore().GetOrgMembership(ctx, u.ID, "o1"); err == nil {
		t.Fatalf("a rank-0 capability mirrored up to org membership %+v — ADR-078 decision 4 forbids it", om)
	}
}

// The boundary a grant needs is the one the role it carries is mirrored up
// to, and a capability OUTSIDE the ladder is mirrored up to none: refusing
// to write it for want of an org membership refuses the state ADR-078
// sanctions, and the one the invitation path creates.
func TestTeamMembers_aConfigEditorGrantIsReRoledToItsOwnCapability(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedConfigEditorGrant(t, s)

	w := newErrorRecorder()
	s.handleUpdateMember(w, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/cfged", `{"role":"config_editor"}`, "t1", "cfged"))
	if w.Code != http.StatusOK {
		t.Fatalf("a role change that keeps the grant OFF the org ladder: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	mb, err := s.authStore().GetMembership(context.Background(), "cfged", "t1")
	if err != nil {
		t.Fatalf("the re-role took back a grant that needs no org membership: %v", err)
	}
	if mb.Role != identity.RoleConfigEditor {
		t.Errorf("the grant no longer carries the capability it was written with: %+v", mb)
	}
}

// The edge the predicate must not cross: a role change onto the LADDER is
// mirrored up to an org membership, so it needs one — and the refusal names
// the account by the address its owner answers at, as the placement's does.
func TestTeamMembers_aConfigEditorGrantCannotClimbTheLadderWithoutTheBoundary(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedConfigEditorGrant(t, s)

	w := newErrorRecorder()
	s.handleUpdateMember(w, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/cfged", `{"role":"member"}`, "t1", "cfged"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a role change onto the ladder with no org membership: code=%d body=%s, want 422", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "cfged@example.com") {
		t.Errorf("the refusal names the account by an opaque id where the placement's names an address: %s", body)
	}
	mb, err := s.authStore().GetMembership(context.Background(), "cfged", "t1")
	if err != nil {
		t.Fatalf("the refused role change revoked the grant: %v", err)
	}
	if mb.Role != identity.RoleConfigEditor {
		t.Errorf("the refused role change wrote the new role anyway: %+v", mb)
	}
}

// The console reports the FACT — this grant has no org membership behind it
// — for any role, and names the sanctioned cause in its own field: the state
// ADR-078 sanctions is a missing boundary with an off-ladder role, and a
// console that dropped the flag for the role lost the row altogether, with
// it any grant an off-boarded holder had re-roled off the ladder.
func TestAdminUserDetail_aConfigEditorGrantNamesTheMissingBoundaryAndWhy(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedConfigEditorGrant(t, s)
	seedOrphanAdminGrant(t, s)

	sanctioned := t1GrantView(t, s, "cfged")
	if !sanctioned.OrphanGrant {
		t.Errorf("the console hides a grant with no org membership behind it: %+v", sanctioned)
	}
	if !sanctioned.OffLadder {
		t.Errorf("the console does not name the role as the sanctioned cause: %+v", sanctioned)
	}
	// And the drift it is NOT: the same missing boundary under a role that is
	// mirrored up to one. A flag that is always on names nothing, so the
	// cause must separate the two rows.
	drift := t1GrantView(t, s, racerID)
	if !drift.OrphanGrant {
		t.Errorf("the #1560 outcome is not flagged: %+v", drift)
	}
	if drift.OffLadder {
		t.Errorf("a LADDER grant is reported as off the ladder: %+v", drift)
	}
}

// orgMembershipDeleteGate fails a delete of the membership row itself: the
// store call BETWEEN the off-boarding's two sweeps, and the one AFTER the
// second. failOnCall names which (0 = every one of them).
type orgMembershipDeleteGate struct {
	identity.Store
	userID, orgID string
	failOnCall    int

	mu    sync.Mutex
	calls int
}

func (g *orgMembershipDeleteGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.DeleteOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	if g.failOnCall == 0 || g.failOnCall == n {
		return errors.New("identity: delete org membership: connection reset by peer")
	}
	return g.Store.DeleteOrgMembership(ctx, userID, orgID)
}

// The off-boarding's THIRD 500. The first sweep went through, so the state
// this exit leaves is not "nothing happened": every grant the sweep found is
// revoked, and since DeleteOne answers nil when nothing matched, the error
// can also be a delete whose ack was lost. Its two siblings name the state
// they left and ask for a re-issue; the body that said only what the store
// said reads as the one exit where nothing has to be finished.
func TestOrgOffboarding_aMembershipDeleteThatFailsNamesWhatTheSweepTook(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()

	real := s.authStore()
	for _, teamID := range []string{"t1", "t2"} {
		if err := real.UpsertMembership(ctx, identity.Membership{
			UserID: racerID, TeamID: teamID, Role: identity.RoleMember, JoinedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// What the body claims about the invitations, as a row rather than a
	// sentence: the first pass went through, so the pending one is gone.
	_, invID := invitationFor(t, s, "t1", "racer@example.com", identity.RoleMember)
	s.authSvc.SetStoreForTest(&orgMembershipDeleteGate{Store: real, userID: racerID, orgID: "o1"})

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("off-boarding whose membership delete failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if inv, err := real.GetInvitation(ctx, invID); err == nil {
		t.Errorf("the body says the pending invitations are revoked, and %+v stands", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("reading the revoked invitation: %v, want ErrNotFound", err)
	}
	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
		t.Fatalf("the membership went although its delete failed — not the scenario this test describes: %v", err)
	}
	for _, teamID := range []string{"t1", "t2"} {
		if mb, err := real.GetMembership(ctx, racerID, teamID); err == nil {
			t.Fatalf("the first sweep left grant %+v — not the scenario this test describes", mb)
		}
	}
	body := w.Diagnostic(t)
	for _, want := range []string{"their team grants in this org are revoked", "with the pending invitations they minted here that carry an org role", "those addressed to them, where their account row named an address", "may or may not be gone", "re-issue the removal"} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 does not say which state it left (missing %q): %s", want, body)
		}
	}
	// The advice this body gives, exercised on the state it left: both passes
	// are idempotent, so the re-issue finishes the removal.
	s.authSvc.SetStoreForTest(real)
	again := newErrorRecorder()
	s.handleRemoveOrgMember(again, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if again.Code != http.StatusNoContent {
		t.Errorf("the re-issue this body asks for: code=%d body=%s, want 204", again.Code, again.Diagnostic(t))
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the re-issued removal left org membership %+v standing", om)
	}
	// And once more on nothing at all: DeleteOrgMembership answers nil when
	// no row matched, which is why the error above can also be a delete
	// whose ack was lost.
	third := newErrorRecorder()
	s.handleRemoveOrgMember(third, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if third.Code != http.StatusNoContent {
		t.Errorf("the removal of a member who holds nothing: code=%d body=%s, want 204", third.Code, third.Diagnostic(t))
	}
}

// ---------------------------------------------------------------------------
// The role a grant ALREADY carries, and the invitations the cascade owes.
// ---------------------------------------------------------------------------

// seedOrphanAdminGrant writes the #1560 outcome this ticket exists to
// prevent: a live grant ON the org ladder with no org membership behind it.
// It goes straight to the store because every production path that could
// still produce it is guarded — which is the point — and it is what
// GET /api/admin/users/{id} reports as orphan_grant.
func seedOrphanAdminGrant(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: racerID, Email: "racer@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if om, err := s.authStore().GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatalf("the fixture is an org member (%+v) — then it is not the orphan state", om)
	}
}

// t1GrantView reads the row the admin console renders for the t1 grant —
// the surface an operator finds this drift on.
func t1GrantView(t *testing.T, s *Server, userID string) adminUserTeamView {
	t.Helper()
	ctx := context.Background()
	u, err := s.authStore().GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.buildAdminUserDetail(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	for _, tv := range view.Teams {
		if tv.TeamID == "t1" {
			return tv
		}
	}
	t.Fatal("the console does not list the t1 grant at all — then this is not the row the test describes")
	return adminUserTeamView{}
}

// orphanGrantFlag is the one field of that row this ticket turns on: a team
// grant with no org membership behind it.
func orphanGrantFlag(t *testing.T, s *Server, userID string) bool {
	t.Helper()
	return t1GrantView(t, s, userID).OrphanGrant
}

// The boundary belongs to the role the grant ALREADY carries as much as to
// the one being written. canManageTeam authorizes on the team grant alone,
// so the holder of an orphan LADDER grant still administers the team they
// were off-boarded from: reading only the role being written would let them
// re-role THEMSELVES onto a capability outside the ladder and keep the grant
// in one request. The invitation door reaches the same state in two, which
// is why the orphan_grant report flags a missing boundary for any role —
// TestTeamMembers_anOrphanGrantLaunderedThroughItsOwnInvitationStaysOnTheDriftReport.
func TestTeamMembers_anOrphanLadderGrantCannotReRoleItselfOffTheLadder(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedOrphanAdminGrant(t, s)
	ctx := context.Background()
	self := auth.Identity{UserID: racerID}
	if !s.canManageTeam(ctx, self, "t1") {
		t.Fatal("the orphan admin grant does not authorize the PATCH — then this is not the request the test describes")
	}
	if !orphanGrantFlag(t, s, racerID) {
		t.Fatal("the console does not flag the orphan grant — then this is not the state the test describes")
	}

	w := newErrorRecorder()
	s.handleUpdateMember(w, teamReq(auth.WithIdentity(ctx, self), "PATCH",
		"/api/teams/t1/members/"+racerID, `{"role":"config_editor"}`, "t1", racerID))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the off-boarded holder re-roling their own orphan grant off the ladder: code=%d body=%s, want 422",
			w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "racer@example.com") {
		t.Errorf("the refusal names the account by an opaque id: %s", body)
	}
	mb, err := s.authStore().GetMembership(ctx, racerID, "t1")
	if err != nil {
		t.Fatalf("the refused role change revoked the grant it was asked to re-role: %v", err)
	}
	if mb.Role != identity.RoleAdmin {
		t.Errorf("the refused role change wrote the new role anyway: %+v", mb)
	}
	if !orphanGrantFlag(t, s, racerID) {
		t.Error("the drift report went quiet on a grant that is still an orphan")
	}
}

// invitationFor mints a team invitation the way the console does.
func invitationFor(t *testing.T, s *Server, teamID, email string, role identity.Role) (token, id string) {
	t.Helper()
	tok, inv, err := s.authSvc.CreateInvitation(context.Background(), teamID, email, role, "teamadmin")
	if err != nil {
		t.Fatalf("mint an invitation for %s on %s: %v", email, teamID, err)
	}
	return tok, inv.ID
}

// A pending invitation is access, the same way a grant is: accepting one
// writes the team grant AND — for any role on the org ladder — the org
// membership the off-boarding just deleted. An invitation left behind is the
// whole removal undone by one request, with no race to win, which is why the
// cascade takes the invitations with the grants. The rows it must NOT take
// are asserted in the same scenario: one addressed to somebody else, one on
// a team of another org, and the ACCEPTED row that records how a member
// joined.
func TestOrgOffboarding_aPendingInvitationIsRevokedWithTheGrants(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	tok, invID := invitationFor(t, s, "t1", "racer@example.com", identity.RoleMember)
	_, otherID := invitationFor(t, s, "t1", "other@example.com", identity.RoleMember)
	_, outsideID := invitationFor(t, s, "t9", "racer@example.com", identity.RoleMember)
	usedTok, usedID := invitationFor(t, s, "t2", "racer@example.com", identity.RoleMember)
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, usedTok); err != nil {
		t.Fatalf("accept the invitation this member joined t2 through: %v", err)
	}
	// A row carrying a MIXED-CASE address, written straight to the store:
	// CreateInvitation normalizes on write, so only a row written by
	// something else can hold one — and both accept paths would honour it,
	// since they normalize on read. The sweep selects with their predicate,
	// so it has to take this one too.
	legacy := identity.Invitation{
		ID: "legacy-mixed-case", TeamID: "t2", Email: "RACER@Example.com", Role: identity.RoleMember,
		TokenHash: auth.HashRefreshToken("legacy-token"), InvitedBy: "teamadmin",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := real.CreateInvitation(ctx, legacy); err != nil {
		t.Fatalf("write the legacy mixed-case invitation: %v", err)
	}
	// A row whose TTL has run out: the accept paths refuse it, and the sweep
	// takes it anyway rather than carry their clock as a second predicate.
	expired := identity.Invitation{
		ID: "expired", TeamID: "t1", Email: "racer@example.com", Role: identity.RoleMember,
		TokenHash: auth.HashRefreshToken("expired-token"), InvitedBy: "teamadmin",
		CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := real.CreateInvitation(ctx, expired); err != nil {
		t.Fatalf("write the expired invitation: %v", err)
	}

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	for _, taken := range []struct{ id, what string }{
		{invID, "the pending invitation addressed to the removed member"},
		{legacy.ID, "the pending invitation whose address differs only in case"},
		{expired.ID, "the pending invitation whose TTL had run out"},
	} {
		if inv, err := real.GetInvitation(ctx, taken.id); err == nil {
			t.Errorf("%s outlived the cascade: %+v", taken.what, inv)
		} else if !errors.Is(err, identity.ErrNotFound) {
			t.Errorf("reading %s: %v, want ErrNotFound", taken.what, err)
		}
	}
	for _, kept := range []struct{ id, what string }{
		{otherID, "an invitation addressed to somebody else"},
		{outsideID, "an invitation on a team outside this org"},
		{usedID, "the accepted invitation that records how they joined"},
	} {
		if _, err := real.GetInvitation(ctx, kept.id); err != nil {
			t.Errorf("the sweep took %s: %v", kept.what, err)
		}
	}
	// The removal that invitation would have undone: accepting it now is
	// refused, and neither row comes back.
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, tok); !errors.Is(err, auth.ErrInvitationNotFound) {
		t.Errorf("accepting the revoked invitation: %v, want ErrInvitationNotFound", err)
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the removed account walked back into the org: %+v", om)
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("the removed account walked back into the team: %+v", mb)
	}
	rows := auditRows(t, s, "t1", invID, "invitation.deleted")
	if ev, ok := rows["invitation.deleted"]; !ok {
		t.Errorf("the revoked invitation left nothing in t1's audit trail: %v", rows)
	} else if ev.Meta["reason"] != "org_member_removed" {
		t.Errorf("the audit row does not name the path that took the invitation: %+v", ev.Meta)
	}
}

// The predicate the sweep selects invitations with is the one the accepts
// gate on (identity.Invitation.AddressedTo), so an invitation the sweep
// leaves behind is one they would refuse, and one it takes is one they would
// have honoured. Both terms of that predicate are read from the accept side
// here, where the sweep's own witness reads them from the other.
func TestTeamInvitations_anAddressIsMatchedAsTheSweepMatchesIt(t *testing.T) {
	t.Run("addressed to somebody else", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedRacer(t, s)
		tok, _ := invitationFor(t, s, "t1", "other@example.com", identity.RoleMember)
		if _, err := s.authSvc.AcceptInvitationForExistingUser(context.Background(), racerID, tok); !errors.Is(err, auth.ErrInvitationMismatch) {
			t.Errorf("accepting an invitation addressed to somebody else: %v, want ErrInvitationMismatch", err)
		}
	})
	t.Run("the address a User row carries is normalized", func(t *testing.T) {
		// The other half of the contract: AddressedTo normalizes ONE side
		// because the row it is given already is. So an invitation minted
		// for a typed-in mixed-case address is accepted by the account whose
		// row carries the normalized one, whichever case the invitation row
		// itself ended up with.
		s := newOrgCredsTestServer(t)
		u, err := s.authStore().CreateUser(context.Background(), identity.User{
			ID: racerID, Email: "RACER@Example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if u.Email != "racer@example.com" {
			t.Errorf("the user row carries %q, not the normalized address", u.Email)
		}
		read, err := s.authStore().GetUser(context.Background(), racerID)
		if err != nil || read.Email != "racer@example.com" {
			t.Errorf("the user row reads back as %q (%v), not the normalized address", read.Email, err)
		}
		tok, _ := invitationFor(t, s, "t1", "RACER@Example.com", identity.RoleMember)
		if _, err := s.authSvc.AcceptInvitationForExistingUser(context.Background(), racerID, tok); err != nil {
			t.Errorf("accepting an invitation minted for a mixed-case address: %v", err)
		}
	})
	t.Run("the same address in another case", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedRacer(t, s)
		// CreateInvitation normalizes on write, so a mixed-case row can only
		// come from something else writing it; the accept normalizes on read
		// and honours it, which is why the sweep must select it too.
		inv := identity.Invitation{
			ID: "legacy-mixed-case", TeamID: "t1", Email: "RACER@Example.com", Role: identity.RoleMember,
			TokenHash: auth.HashRefreshToken("legacy-token"), InvitedBy: "teamadmin",
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}
		if err := s.authStore().CreateInvitation(context.Background(), inv); err != nil {
			t.Fatal(err)
		}
		mb, err := s.authSvc.AcceptInvitationForExistingUser(context.Background(), racerID, "legacy-token")
		if err != nil {
			t.Fatalf("accepting an invitation whose address differs only in case: %v", err)
		}
		if mb.TeamID != "t1" || mb.Role != identity.RoleMember {
			t.Errorf("the accepted grant: %+v", mb)
		}
	})
}

// offboardingWindowGate mints a t1 invitation for the removed member once
// their org membership row is deleted — a team admin acting inside the
// window the second sweep exists for. With acceptInSecondSweep it also
// accepts that invitation from INSIDE the second sweep, right after the pass
// has read the grants it would revoke: the interleaving that decides whether
// the invitations may be swept after the grants.
type offboardingWindowGate struct {
	identity.Store
	s             *Server
	userID, email string

	acceptInSecondSweep bool

	mu        sync.Mutex
	minted    bool
	lists     int
	token     string
	acceptErr error
}

func (g *offboardingWindowGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if err := g.Store.DeleteOrgMembership(ctx, userID, orgID); err != nil {
		return err
	}
	if userID != g.userID {
		return nil
	}
	g.mu.Lock()
	first := !g.minted
	g.minted = true
	g.mu.Unlock()
	if !first {
		return nil
	}
	tok, _, err := g.s.authSvc.CreateInvitation(context.Background(), "t1", g.email, identity.RoleMember, "teamadmin")
	if err != nil {
		return fmt.Errorf("mint the in-window invitation: %w", err)
	}
	g.mu.Lock()
	g.token = tok
	g.mu.Unlock()
	return nil
}

func (g *offboardingWindowGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	held, err := g.Store.ListMembershipsByUser(ctx, userID)
	if userID != g.userID || !g.acceptInSecondSweep {
		return held, err
	}
	g.mu.Lock()
	g.lists++
	n := g.lists
	tok := g.token
	g.mu.Unlock()
	// The second sweep's grant pass has just read what it would revoke; an
	// accept landing now writes a grant behind it.
	if n == 2 && tok != "" {
		_, aerr := g.s.authSvc.AcceptInvitationForExistingUser(context.Background(), g.userID, tok)
		g.mu.Lock()
		g.acceptErr = aerr
		g.mu.Unlock()
	}
	return held, err
}

// An invitation minted INSIDE the window is swept exactly as a grant written
// there is: the second pass re-reads each team's invitations, so the mint a
// team admin issued while the removal was in flight does not outlive it.
func TestOrgOffboarding_anInvitationMintedInsideTheWindowIsSweptToo(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	gate := &offboardingWindowGate{Store: real, s: s, userID: racerID, email: "racer@example.com"}
	s.authSvc.SetStoreForTest(gate)

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	gate.mu.Lock()
	tok := gate.token
	gate.mu.Unlock()
	if tok == "" {
		t.Fatal("nothing was minted inside the window — then this is not the race the test describes")
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, tok); !errors.Is(err, auth.ErrInvitationNotFound) {
		t.Errorf("the invitation minted inside the window survived the second sweep: %v, want ErrInvitationNotFound", err)
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the removed account walked back into the org: %+v", om)
	}
}

// Why the invitations go FIRST in each pass: the grant pass revokes what it
// READ, so an accept landing after that read writes a grant behind it and
// the pass is already past. Revoking the invitations first puts the two
// sides in the same order, since an accept CLAIMS the invitation before it
// writes any access: an accept then survives a pass only by claiming before
// its first delete AND writing the grant after its last. The accept here is
// driven from inside the second sweep, after its grant listing, so it has
// neither half — the claim on the row the first pass took answers
// ErrInvitationNotFound and writes nothing.
func TestOrgOffboarding_theSweepRevokesTheInvitationsBeforeTheGrants(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	gate := &offboardingWindowGate{
		Store: real, s: s, userID: racerID, email: "racer@example.com", acceptInSecondSweep: true,
	}
	s.authSvc.SetStoreForTest(gate)

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	gate.mu.Lock()
	tok, aerr, lists := gate.token, gate.acceptErr, gate.lists
	gate.mu.Unlock()
	if tok == "" || lists < 2 {
		t.Fatalf("the window this test imposes no longer exists: minted=%q grant listings=%d", tok, lists)
	}
	if !errors.Is(aerr, auth.ErrInvitationNotFound) {
		t.Errorf("the accept inside the second sweep went through: %v, want ErrInvitationNotFound", aerr)
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("an accept landing after the grant pass left grant %+v standing", mb)
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("an accept landing after the grant pass wrote the org membership back: %+v", om)
	}
}

// invitationListErrorGate fails one named listing of a team's invitations —
// the read the sweep needs before it can revoke any of them — on the pass the
// test names. failUserRead instead fails the read of the address those
// invitations are addressed to.
type invitationListErrorGate struct {
	identity.Store
	teamID       string
	failOnCall   int
	failUserRead string

	mu    sync.Mutex
	lists int
}

func (g *invitationListErrorGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	if teamID != g.teamID {
		return g.Store.ListInvitationsByTeam(ctx, teamID)
	}
	g.mu.Lock()
	g.lists++
	n := g.lists
	g.mu.Unlock()
	if n == g.failOnCall {
		return nil, errors.New("identity: list invitations by team: connection reset by peer")
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

func (g *invitationListErrorGate) GetUser(ctx context.Context, id string) (identity.User, error) {
	if g.failUserRead != "" && id == g.failUserRead {
		return identity.User{}, errors.New("identity: get user: connection reset by peer")
	}
	return g.Store.GetUser(ctx, id)
}

// A sweep that cannot READ the invitations has not finished the cascade, and
// says so. Each pass owes its own scenario: the FIRST stops before the
// membership row is touched, so the member is intact and the next move is the
// re-issue the body asks for; the SECOND leaves the member removed and
// possibly still holding the invitation its body names. A read error
// swallowed on either would report a completed removal over an invitation
// that still lets the member back in. A user row that cannot be read is the
// same kind of unfinished cascade: the address is what the sweep selects on.
func TestOrgOffboarding_anUnreadableInvitationListStopsTheCascade(t *testing.T) {
	seed := func(t *testing.T) (*Server, identity.Store, string) {
		t.Helper()
		s := newOrgCredsTestServer(t)
		seedRacer(t, s)
		real := s.authStore()
		if err := real.UpsertMembership(context.Background(), identity.Membership{
			UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		_, invID := invitationFor(t, s, "t1", "racer@example.com", identity.RoleMember)
		return s, real, invID
	}
	off := func(t *testing.T, s *Server) *errorRecorder {
		t.Helper()
		w := newErrorRecorder()
		s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("off-boarding whose invitation read failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
		}
		return w
	}
	wants := func(t *testing.T, body string, phrases ...string) {
		t.Helper()
		for _, want := range phrases {
			if !strings.Contains(body, want) {
				t.Errorf("the 500 does not say what failed and what state it left (missing %q): %s", want, body)
			}
		}
	}
	ctx := context.Background()

	t.Run("the first pass cannot list them", func(t *testing.T) {
		s, real, invID := seed(t)
		s.authSvc.SetStoreForTest(&invitationListErrorGate{Store: real, teamID: "t1", failOnCall: 1})
		body := off(t, s).Diagnostic(t)
		wants(t, body, "list team t1 invitations", "org membership stands", "re-issue the removal")
		if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
			t.Errorf("the org membership went although the sweep before it failed: %v", err)
		}
		if _, err := real.GetInvitation(ctx, invID); err != nil {
			t.Errorf("the invitation went although its own listing failed: %v", err)
		}
	})

	t.Run("the second pass cannot list them", func(t *testing.T) {
		s, real, _ := seed(t)
		s.authSvc.SetStoreForTest(&invitationListErrorGate{Store: real, teamID: "t1", failOnCall: 2})
		body := off(t, s).Diagnostic(t)
		wants(t, body, "list team t1 invitations", "may still hold a team grant in this org", "re-issue the removal")
		// The last sweep runs after this failure and went through, so the
		// body names the grants alone: naming the invitation residue too
		// would send an operator after a row that sweep took.
		if strings.Contains(body, standingMintedInvitation) {
			t.Errorf("the 500 of a failed grant pass names an invitation residue the last sweep took: %s", body)
		}
		if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
			t.Errorf("the second pass runs once the membership row is gone: %+v", om)
		}
	})

	t.Run("the address the invitations are addressed to cannot be read", func(t *testing.T) {
		s, real, invID := seed(t)
		s.authSvc.SetStoreForTest(&invitationListErrorGate{Store: real, teamID: "t1", failUserRead: racerID})
		body := off(t, s).Diagnostic(t)
		wants(t, body, "resolve the address invitations of racer are addressed to", "org membership stands", "re-issue the removal")
		if _, err := real.GetInvitation(ctx, invID); err != nil {
			t.Errorf("the invitation went although the address read failed: %v", err)
		}
	})
}

// concurrentInvitationRevokeGate answers the invitation delete the way both
// stores answer a row that is no longer there: ErrNotFound. It models the
// ordinary concurrency — an admin revoking the invitation by hand, or the
// off-boarding's own other pass — between the sweep's listing and its delete.
type concurrentInvitationRevokeGate struct {
	identity.Store
	invID string
}

func (g *concurrentInvitationRevokeGate) DeleteInvitation(ctx context.Context, id string) error {
	if id == g.invID {
		if err := g.Store.DeleteInvitation(ctx, id); err != nil {
			return err
		}
		return identity.ErrNotFound
	}
	return g.Store.DeleteInvitation(ctx, id)
}

// An invitation somebody else revoked between the listing and the delete is
// already in the state this sweep wants, and both stores say so definitively
// (ErrNotFound, where DeleteMembership answers nil whatever it found). So the
// cascade finishes — a 500 there would send the operator to re-issue a
// removal that is done — and the trail carries no line for a row this pass
// did not take.
func TestOrgOffboarding_anInvitationRevokedUnderTheSweepIsNotAFailure(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_, invID := invitationFor(t, s, "t1", "racer@example.com", identity.RoleMember)
	s.authSvc.SetStoreForTest(&concurrentInvitationRevokeGate{Store: real, invID: invID})

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding whose invitation was revoked under it: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Error("the cascade stopped on a row that was already gone")
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("the grant pass after the invitations never ran: %+v", mb)
	}
	if rows := auditRows(t, s, "t1", invID); len(rows) != 0 {
		t.Errorf("the trail names a revoke this pass did not make: %v", rows)
	}
}

// subjectReadGate answers the compensation's re-read only at its deadline —
// the stepdown or the saturated pool the detached context exists for — and,
// like MongoStore (whose GetUser passes the context to FindOne), refuses any
// call whose context is already done. It also counts the subject reads that
// arrive with NO deadline at all, which is what resolving the label on the
// request's own context would produce.
type subjectReadGate struct {
	identity.Store
	userID, orgID, teamID string
	gone                  bool

	mu sync.Mutex

	// wrote marks the grant write this scenario is about — the named account
	// in the named team: the read it cannot serve is the compensation's,
	// which FOLLOWS that write. Keyed on the write rather than on a read
	// count — the handler takes reads of the same row before it (the boundary
	// pre-check, the rank guard), and an ordinal moves with them. It is held
	// under mu with the counters, so one decision reads one snapshot;
	// postWriteReads counts the reads after that write, of which exactly the
	// FIRST is the compensation's.
	wrote                  bool
	postWriteReads         int
	subjectReads           int
	subjectReadsOnDoneCtx  int
	subjectReadsNoDeadline int
	subjectBudgets         []time.Duration
}

func (g *subjectReadGate) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	err := g.Store.UpsertMembership(ctx, mb)
	if mb.UserID == g.userID && mb.TeamID == g.teamID {
		g.mu.Lock()
		g.wrote = true
		g.mu.Unlock()
	}
	return err
}

func (g *subjectReadGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	wrote := g.wrote
	if wrote {
		g.postWriteReads++
	}
	postWrite := g.postWriteReads
	g.mu.Unlock()
	// Every read but the first one after the write is served normally: the
	// reads before it are the handler's own, and the ones after the
	// compensation's are whatever the test drives next.
	if !wrote || postWrite > 1 {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	if _, ok := ctx.Deadline(); !ok {
		return identity.OrgMembership{}, errors.New("identity: the re-read carries no deadline to spend")
	}
	<-ctx.Done()
	if g.gone {
		return identity.OrgMembership{}, identity.ErrNotFound
	}
	return identity.OrgMembership{}, fmt.Errorf("identity: get org membership: %w", ctx.Err())
}

func (g *subjectReadGate) GetUser(ctx context.Context, id string) (identity.User, error) {
	// Only the reads from INSIDE the compensation are the label's: the
	// placement loads the user before writing the grant, on the request's own
	// context, and counting that one would flag the handler's ordinary read.
	g.mu.Lock()
	inCompensation := id == g.userID && g.wrote
	// The compensation reads the account TWICE, in this order: the label
	// first (grantSubject, for the refusal's subject), then the address the
	// sweep of the rows kept on the revoked grant selects by
	// (revokeInvitationsKeptOnTeamGrant). Only the first is the label's, and
	// only its budget is this test's subject — the sweep is a repair, priced
	// like the repair it is.
	if inCompensation {
		g.subjectReads++
	}
	if inCompensation && g.subjectReads == 1 {
		if dl, ok := ctx.Deadline(); ok {
			g.subjectBudgets = append(g.subjectBudgets, time.Until(dl))
		} else {
			g.subjectReadsNoDeadline++
		}
		if ctx.Err() != nil {
			g.subjectReadsOnDoneCtx++
		}
	}
	g.mu.Unlock()
	if inCompensation {
		if err := ctx.Err(); err != nil {
			return identity.User{}, fmt.Errorf("identity: get user: %w", err)
		}
	}
	return g.Store.GetUser(ctx, id)
}

func (g *subjectReadGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("identity: delete membership: %w", err)
	}
	return g.Store.DeleteMembership(ctx, userID, teamID)
}

// The refusal names the account by the address its owner answers at even
// when the diagnosis spent its whole budget getting there — the very
// incident this guard exists for. A label read on the budget the re-read
// already spent is dead on arrival (MemoryStore cannot see it: its GetUser
// ignores the context), and one on the REQUEST's context carries no deadline
// at all and would hold the revoke behind a store that cannot answer who the
// subject is. So the label is read last, on a budget of its own — and that
// budget is sized for a label: the store that answered neither the re-read
// nor the revoke answers the label no better, and a repair-sized budget
// there holds the request for a third full budget in series to buy the
// account id the fallback already has.
func TestOrgOffboarding_theRefusalNamesItsSubjectOnABudgetOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		gone bool
		want int
	}{
		{"the boundary is gone (409)", true, http.StatusConflict},
		{"the boundary is unreadable (500)", false, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacer(t, s)
			gate := &subjectReadGate{Store: s.authStore(), userID: racerID, orgID: "o1", teamID: "t1", gone: tc.gone}
			s.authSvc.SetStoreForTest(gate)

			w := newErrorRecorder()
			s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT",
				"/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
			if w.Code != tc.want {
				t.Fatalf("code=%d body=%s, want %d", w.Code, w.Diagnostic(t), tc.want)
			}
			if body := w.Diagnostic(t); !strings.Contains(body, "racer@example.com") {
				t.Errorf("the refusal names the account by an opaque id: %s", body)
			}
			gate.mu.Lock()
			done, nodl := gate.subjectReadsOnDoneCtx, gate.subjectReadsNoDeadline
			gate.mu.Unlock()
			if done != 0 {
				t.Errorf("%d subject read(s) ran on a context already done — a store that honours it answers nothing", done)
			}
			if nodl != 0 {
				t.Errorf("%d subject read(s) ran with no deadline — an unbounded label read holds the repair it describes", nodl)
			}
			gate.mu.Lock()
			budgets := append([]time.Duration(nil), gate.subjectBudgets...)
			gate.mu.Unlock()
			if len(budgets) == 0 {
				t.Fatal("the label was never read inside the compensation — then this is not the read the test describes")
			}
			for _, b := range budgets {
				if b > grantSubjectTimeout {
					t.Errorf("the label arrived with %s of budget, want at most grantSubjectTimeout=%s — a label priced like the repair costs the request a third budget in series, for the id grantSubject falls back to anyway",
						b, grantSubjectTimeout)
				}
			}
			if _, err := s.authStore().GetMembership(context.Background(), racerID, "t1"); err == nil {
				t.Error("the grant the guard refused was left standing")
			}
		})
	}
}

// What an org membership with NO team grant confers, which is the state the
// invitation accepts leave behind when the team grant cannot be written:
// org-scoped READ — the roster, the settings, the usage, the team and
// SSO-provider lists — and no team access. The accepts write the boundary
// FIRST because of that comparison, and the comparison is this test.
func TestOrgMembership_withNoTeamGrantConfersOrgScopedReadOnly(t *testing.T) {
	s := newOrgCredsTestServer(t)
	// The SSO route is registered only where the store is wired
	// (server_routes.go), so the gate is read through a wired one.
	s.orgSSO = orgsso.NewMemoryStore()
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "lonely", Email: "lonely@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "lonely", OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if ms, err := s.authStore().ListMembershipsByUser(ctx, "lonely"); err != nil || len(ms) != 0 {
		t.Fatalf("the fixture holds team grants %+v (%v) — then it is not the state this test describes", ms, err)
	}
	me := auth.WithIdentity(ctx, auth.Identity{UserID: "lonely"})

	for _, tc := range []struct {
		name string
		h    func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"the org roster", s.handleListOrgMembers, "/api/orgs/o1/members"},
		{"the org settings", s.handleGetOrgSettings, "/api/orgs/o1/settings"},
		{"the org usage", s.handleOrgUsage, "/api/orgs/o1/usage"},
		{"the org's teams", s.handleListOrgTeams, "/api/orgs/o1/teams"},
		{"the org itself", s.handleGetOrg, "/api/orgs/o1"},
		{"the org's SSO providers", s.handleListOrgSSOProviders, "/api/orgs/o1/sso/providers"},
	} {
		w := newErrorRecorder()
		tc.h(w, orgReq(me, "GET", tc.path, "", "o1"))
		if w.Code != http.StatusOK {
			t.Errorf("%s for an org member with no team: code=%d body=%s, want 200", tc.name, w.Code, w.Body.String())
		}
	}
	// And no team access: the roster of a team of that same org is refused.
	w := newErrorRecorder()
	s.handleListTeamMembers(w, teamReq(me, "GET", "/api/teams/t1/members", "", "t1", ""))
	if w.Code != http.StatusForbidden {
		t.Errorf("the t1 roster for an org member with no grant on it: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// An accept racing the off-boarding: which of the two writes first.
// ---------------------------------------------------------------------------

// acceptUnderTheSweepGate pauses the accept right after it has READ the
// pending invitation row, and releases it once the off-boarding's SECOND
// pass has taken the grant listing it will revoke. Nothing orders those two
// store calls, so this is the interleaving the invitations-first order does
// not cover on its own: the sweep deletes the row the accept is holding, and
// the accept reaches its own writes behind both passes.
type acceptUnderTheSweepGate struct {
	identity.Store
	userID string

	reached chan struct{}
	release chan struct{}

	mu     sync.Mutex
	looked bool
	lists  int
}

func (g *acceptUnderTheSweepGate) GetInvitationByTokenHash(ctx context.Context, h string) (identity.Invitation, error) {
	inv, err := g.Store.GetInvitationByTokenHash(ctx, h)
	g.mu.Lock()
	first := !g.looked
	g.looked = true
	g.mu.Unlock()
	if !first {
		return inv, err
	}
	close(g.reached)
	<-g.release
	return inv, err
}

func (g *acceptUnderTheSweepGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	held, err := g.Store.ListMembershipsByUser(ctx, userID)
	if userID != g.userID {
		return held, err
	}
	g.mu.Lock()
	g.lists++
	n := g.lists
	g.mu.Unlock()
	if n == 2 {
		// The second pass holds the list of grants it will revoke: anything
		// written from here on is behind both sweeps.
		close(g.release)
	}
	return held, err
}

// An accept holding a row the sweep revokes writes NO ACCESS, because the
// claim of the invitation is its first access write and both stores refuse
// it on a row that is gone (a registration has written its account row
// before that point, which is not access). Claiming last instead made the
// accept's two access rows
// land behind the whole removal: a 204 that left the off-boarded account an
// org member AND a team grant holder, reading the org roster, while the
// accept itself answered that it had failed.
func TestOrgOffboarding_anAcceptHoldingTheRevokedRowWritesNothing(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	tok, invID := invitationFor(t, s, "t1", "racer@example.com", identity.RoleMember)
	gate := &acceptUnderTheSweepGate{
		Store: real, userID: racerID,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	s.authSvc.SetStoreForTest(gate)

	var acceptErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, acceptErr = s.authSvc.AcceptInvitationForExistingUser(context.Background(), racerID, tok)
	}()
	awaitGate(t, gate.reached, "the accept reading the pending invitation")

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	awaitGate(t, done, "the accept returning")

	if _, err := real.GetInvitation(ctx, invID); err == nil {
		t.Fatal("the sweep never revoked the invitation — then this is not the interleaving the test describes")
	}
	if !errors.Is(acceptErr, auth.ErrInvitationNotFound) {
		t.Errorf("the accept answered %v, want ErrInvitationNotFound — a claim on a revoked row is the write that must fail", acceptErr)
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the off-boarded account is an ORG MEMBER again after a 204: %+v", om)
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("the off-boarded account holds a LIVE TEAM GRANT after a 204: %+v", mb)
	}
	roster := newErrorRecorder()
	s.handleListOrgMembers(roster, orgReq(auth.WithIdentity(ctx, auth.Identity{UserID: racerID}), "GET", "/api/orgs/o1/members", "", "o1"))
	if roster.Code == http.StatusOK {
		t.Errorf("the off-boarded account reads the org roster: code=%d", roster.Code)
	}
}

// claimedAcceptGate drives the interleaving the claim does NOT close, which
// is the one the last delete of the membership row exists for: an accept
// that claimed its invitation BEFORE the sweep reached it, and writes its
// two access rows after the second pass has read the grants it will revoke.
// It pauses the accept inside its own claim, so the accept runs its
// production path either side of the pause, and holds the sweep until those
// writes have landed, so the order is the test's and not the scheduler's.
type claimedAcceptGate struct {
	identity.Store
	userID string

	claimed chan struct{}
	release chan struct{}
	done    chan struct{}

	mu     sync.Mutex
	claims int
	lists  int
}

func (g *claimedAcceptGate) UpdateInvitation(ctx context.Context, inv identity.Invitation) error {
	err := g.Store.UpdateInvitation(ctx, inv)
	g.mu.Lock()
	g.claims++
	first := g.claims == 1
	g.mu.Unlock()
	if !first {
		return err
	}
	close(g.claimed)
	<-g.release
	return err
}

func (g *claimedAcceptGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	held, err := g.Store.ListMembershipsByUser(ctx, userID)
	if userID != g.userID {
		return held, err
	}
	g.mu.Lock()
	g.lists++
	n := g.lists
	g.mu.Unlock()
	if n == 2 {
		// The grants this pass will revoke are read: let the accept write
		// its own behind them, and wait for it, so the pass walks past a
		// grant that is already there.
		close(g.release)
		<-g.done
	}
	return held, err
}

// What a race the accept wins THIS way leaves: a team grant with no
// boundary, which the console reports. The accept straddles the second
// pass, so its grant survives the sweep; the removal's last delete takes
// the org membership it wrote back, and the accept writes the boundary
// BEFORE the grant, so keeping both takes landing after that delete — the
// other interleaving, which
// anAcceptThatLandsBehindTheLastDeleteIsASilentReAdmission witnesses.
func TestOrgOffboarding_anAcceptThatStraddlesTheSweepKeepsNoBoundary(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	tok, invID := invitationFor(t, s, "t1", "racer@example.com", identity.RoleMember)
	gate := &claimedAcceptGate{
		Store: real, userID: racerID,
		claimed: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}),
	}
	s.authSvc.SetStoreForTest(gate)

	var acceptErr error
	go func() {
		defer close(gate.done)
		_, acceptErr = s.authSvc.AcceptInvitationForExistingUser(context.Background(), racerID, tok)
	}()
	awaitGate(t, gate.claimed, "the accept claiming its invitation")

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	awaitGate(t, gate.done, "the accept returning")

	if acceptErr != nil {
		t.Fatalf("the accept failed (%v) — then this is not the race the test describes", acceptErr)
	}
	// The sweep walks past an invitation someone has already accepted: it is
	// the trail of how the member joined, and both accepts refuse it.
	if inv, err := real.GetInvitation(ctx, invID); err != nil || inv.AcceptedAt == nil {
		t.Fatalf("the claimed invitation is not the accepted trail the sweep leaves: %+v (%v)", inv, err)
	}
	if _, err := real.GetMembership(ctx, racerID, "t1"); err != nil {
		t.Fatalf("the accept's grant did not survive the sweep (%v) — then this is not the race the test describes", err)
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the accept walked the removed account back into the ORG: %+v — the removal's last delete is what takes that row", om)
	}
	if !orphanGrantFlag(t, s, racerID) {
		t.Error("the grant that survived the race is not reported as drift — then nothing names the access the race left")
	}
	roster := newErrorRecorder()
	s.handleListOrgMembers(roster, orgReq(auth.WithIdentity(ctx, auth.Identity{UserID: racerID}), "GET", "/api/orgs/o1/members", "", "o1"))
	if roster.Code == http.StatusOK {
		t.Errorf("the off-boarded account reads the org roster: code=%d", roster.Code)
	}
}

// The 422 of a role change keeps the holder of an orphan LADDER grant from
// re-roling it off the ladder in ONE request; it does not keep them from the
// same grant state in TWO, since canManageTeam authorizes minting an
// invitation to their own address and an off-ladder accept writes no
// boundary. What must not happen is the drift going quiet: the console
// reports the missing org membership for any role, and names the off-ladder
// role beside it as the sanctioned cause.
func TestTeamMembers_anOrphanGrantLaunderedThroughItsOwnInvitationStaysOnTheDriftReport(t *testing.T) {
	s := newOrgCredsTestServer(t)
	// This fixture attaches the auth service after New, so the seams the
	// constructor registers in production are registered here: the accept
	// below is a lowering write (config_editor over an admin grant), and the
	// reconcile it owes takes nothing — no cap measures the role it writes.
	s.wireTeamGrantSweep()
	s.wireTeamRankReconcile()
	seedOrphanAdminGrant(t, s)
	ctx := context.Background()
	self := auth.Identity{UserID: racerID}
	selfCtx := auth.WithIdentity(ctx, self)
	if !orphanGrantFlag(t, s, racerID) {
		t.Fatal("the console does not flag the orphan grant — then this is not the state the test describes")
	}

	patch := newErrorRecorder()
	s.handleUpdateMember(patch, teamReq(selfCtx, "PATCH",
		"/api/teams/t1/members/"+racerID, `{"role":"config_editor"}`, "t1", racerID))
	if patch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the one-request door: code=%d body=%s, want 422", patch.Code, patch.Body.String())
	}

	mint := newErrorRecorder()
	s.handleCreateInvitation(mint, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
		`{"email":"racer@example.com","role":"config_editor"}`, "t1", ""))
	if mint.Code != http.StatusOK {
		t.Fatalf("minting an invitation for one's own address: code=%d body=%s", mint.Code, mint.Body.String())
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(mint.Body.Bytes(), &minted); err != nil || minted.Token == "" {
		t.Fatalf("the mint answered no token: %v %s", err, mint.Body.String())
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, minted.Token); err != nil {
		t.Fatalf("accepting one's own invitation: %v", err)
	}

	view := t1GrantView(t, s, racerID)
	if view.Role != string(identity.RoleConfigEditor) {
		t.Fatalf("the grant was not re-roled off the ladder: %+v — then this is not the state the test describes", view)
	}
	if om, err := s.authStore().GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatalf("the accept wrote an org membership (%+v) — then the grant is no orphan and this is not the state the test describes", om)
	}
	if !view.OrphanGrant {
		t.Errorf("the drift report went quiet on a grant with no org membership: %+v", view)
	}
	if !view.OffLadder {
		t.Errorf("the drift report does not name the sanctioned cause of the missing boundary: %+v", view)
	}
}

// The off-boarding's FOURTH 500: the LAST delete of the membership row, the
// one that takes back a boundary an accept wrote inside the window. Both
// sweeps are through when it fails, so the body has to say that the member
// may be an org member again — and the re-issue it asks for is exercised on
// exactly that state.
func TestOrgOffboarding_aLastDeleteThatFailsSaysTheMemberMayBeBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.authSvc.SetStoreForTest(&orgMembershipDeleteGate{Store: real, userID: racerID, orgID: "o1", failOnCall: 2})

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("off-boarding whose last delete failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Fatalf("the sweeps did not go through (%+v) — then this is not the exit the test describes", mb)
	}
	body := w.Diagnostic(t)
	for _, want := range []string{"re-deleting their org membership failed", "may stand", "they would be an org member again", "re-issue the removal"} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 does not say which state it left (missing %q): %s", want, body)
		}
	}
	// The state it left, as the body describes it: an accept wrote the
	// boundary back and the last delete could not take it.
	if err := real.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: racerID, OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.authSvc.SetStoreForTest(real)
	again := newErrorRecorder()
	s.handleRemoveOrgMember(again, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if again.Code != http.StatusNoContent {
		t.Errorf("the re-issue this body asks for: code=%d body=%s, want 204", again.Code, again.Diagnostic(t))
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the re-issued removal left org membership %+v standing", om)
	}
}

// stalledClaimGate holds an accept INSIDE its claim of the invitation: the
// claim has landed, so the sweep that lists the row after it skips the
// accepted trail, and the two access writes land wherever the test lets
// them — here, behind the removal's last delete.
type stalledClaimGate struct {
	identity.Store

	claimed chan struct{}
	release chan struct{}

	mu     sync.Mutex
	claims int
}

func (g *stalledClaimGate) UpdateInvitation(ctx context.Context, inv identity.Invitation) error {
	err := g.Store.UpdateInvitation(ctx, inv)
	g.mu.Lock()
	g.claims++
	first := g.claims == 1
	g.mu.Unlock()
	if !first {
		return err
	}
	close(g.claimed)
	<-g.release
	return err
}

// What the last delete does NOT buy, asserted so the sentence that says so
// can be read against it: an accept that claimed its invitation before the
// first sweep and lands BOTH its access writes after the last delete keeps
// the grant AND the boundary — the #1560 outcome, with orphan_grant quiet
// because the boundary is there, so nothing reports it. The last delete
// bounds the WINDOW (an accept that straddles the sweeps loses its
// boundary — anAcceptThatStraddlesTheSweepKeepsNoBoundary), not the race;
// closing the race takes a conditional boundary write, which
// identity.Store has no seam for. The repair the docs name IS exercised
// here: re-issuing the removal takes both rows.
func TestOrgOffboarding_anAcceptThatLandsBehindTheLastDeleteIsASilentReAdmission(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	tok, invID := invitationFor(t, s, "t1", "racer@example.com", identity.RoleMember)
	gate := &stalledClaimGate{Store: real, claimed: make(chan struct{}), release: make(chan struct{})}
	s.authSvc.SetStoreForTest(gate)

	var acceptErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, acceptErr = s.authSvc.AcceptInvitationForExistingUser(context.Background(), racerID, tok)
	}()
	awaitGate(t, gate.claimed, "the accept claiming its invitation")

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatalf("the membership stood before the accept resumed (%+v) — then this is not the order the test describes", om)
	}

	close(gate.release)
	awaitGate(t, done, "the accept returning")
	if acceptErr != nil {
		t.Fatalf("the accept failed (%v) — then this is not the order the test describes", acceptErr)
	}
	if inv, err := real.GetInvitation(ctx, invID); err != nil || inv.AcceptedAt == nil {
		t.Fatalf("the claimed invitation is not the accepted trail the sweep skipped: %+v err=%v", inv, err)
	}

	om, omErr := real.GetOrgMembership(ctx, racerID, "o1")
	mb, mbErr := real.GetMembership(ctx, racerID, "t1")
	if omErr != nil || mbErr != nil {
		t.Fatalf("the accept did not write both rows behind the removal (membership %+v err=%v, grant %+v err=%v) — then this is not the residual the docs describe", om, omErr, mb, mbErr)
	}
	// Both rows: an org member holding a team grant, which is why the
	// console's row for it carries NO drift flag — orphan_grant is a grant
	// with no boundary and this one's boundary came back. All three flags
	// are read, since what the docs claim goes unreported is the whole row,
	// not the one field this ticket turns on.
	if view := t1GrantView(t, s, racerID); view.OrphanGrant || view.OffLadder || view.MissingTeam {
		t.Errorf("the console flags the grant of a silently re-admitted member: %+v", view)
	}
	roster := newErrorRecorder()
	s.handleListOrgMembers(roster, orgReq(auth.WithIdentity(ctx, auth.Identity{UserID: racerID}), "GET", "/api/orgs/o1/members", "", "o1"))
	if roster.Code != http.StatusOK {
		t.Errorf("the re-admitted account reading the org roster: code=%d, want 200 — the membership row is back", roster.Code)
	}

	// The repair the org-member DELETE row names, on the state it left.
	again := newErrorRecorder()
	s.handleRemoveOrgMember(again, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if again.Code != http.StatusNoContent {
		t.Fatalf("the re-issued removal: code=%d body=%s, want 204", again.Code, again.Body.String())
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the re-issued removal left org membership %+v standing", om)
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("the re-issued removal left team grant %+v standing", mb)
	}
}

// lastDeleteBudgetGate reads, from inside the store, the budget each call of
// the off-boarding's detached tail arrives with: the second sweep's listing
// SPENDS part of its own, and the deadline the delete after it carries is
// what says whether that delete was priced separately.
type lastDeleteBudgetGate struct {
	identity.Store
	userID string

	spendInSecondSweep time.Duration

	mu              sync.Mutex
	lists           int
	sweepDeadline   time.Time
	deleteDeadlines []time.Time
	deleteRemaining []time.Duration
}

func (g *lastDeleteBudgetGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	held, err := g.Store.ListMembershipsByUser(ctx, userID)
	if userID != g.userID {
		return held, err
	}
	g.mu.Lock()
	g.lists++
	n := g.lists
	if n == 2 {
		if d, ok := ctx.Deadline(); ok {
			g.sweepDeadline = d
		}
	}
	g.mu.Unlock()
	if n == 2 && g.spendInSecondSweep > 0 {
		// A store that answers, but slowly. MemoryStore ignores ctx, so the
		// sweep still SUCCEEDS: what it spends is the point, not whether it
		// fails.
		time.Sleep(g.spendInSecondSweep)
	}
	return held, err
}

func (g *lastDeleteBudgetGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if userID == g.userID {
		d, _ := ctx.Deadline()
		g.mu.Lock()
		g.deleteDeadlines = append(g.deleteDeadlines, d)
		if d.IsZero() {
			g.deleteRemaining = append(g.deleteRemaining, 0)
		} else {
			g.deleteRemaining = append(g.deleteRemaining, time.Until(d))
		}
		g.mu.Unlock()
	}
	return g.Store.DeleteOrgMembership(ctx, userID, orgID)
}

// The last delete of the membership row is a REPAIR, so it is priced like
// one: its own grantCompensationTimeout, not the remainder of what the
// second sweep before it already spent. Sharing that budget made a
// slow-but-working store answer 500 on an off-boarding that had gone
// through in full — a body naming an org membership that may stand, where
// nothing had been written. The budget is read from inside the store rather
// than measured in wall clock: the harm costs one whole budget of sleep to
// reproduce, the cause is instant.
func TestOrgOffboarding_theLastDeleteGetsItsOwnBudget(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	const spent = 300 * time.Millisecond
	gate := &lastDeleteBudgetGate{Store: real, userID: racerID, spendInSecondSweep: spent}
	s.authSvc.SetStoreForTest(gate)

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding against a slow-but-working store: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.deleteDeadlines) != 2 {
		t.Fatalf("want 2 DeleteOrgMembership calls on the target, got %d", len(gate.deleteDeadlines))
	}
	if gate.sweepDeadline.IsZero() {
		t.Fatal("the second sweep ran with no deadline — then this is not the detached tail the test describes")
	}
	if gate.deleteDeadlines[1].IsZero() {
		t.Fatal("the last delete ran with no deadline at all")
	}
	if gate.deleteDeadlines[1].Equal(gate.sweepDeadline) {
		t.Errorf("the last delete runs on the deadline the second sweep already spent %v of (remaining %v of %v)",
			spent, gate.deleteRemaining[1], grantCompensationTimeout)
	}
	if left := gate.deleteRemaining[1]; left <= grantCompensationTimeout-spent {
		t.Errorf("the last delete arrived with %v of budget, want more than %v — a budget of its own",
			left, grantCompensationTimeout-spent)
	}
}

// windowPlacementGate issues a legitimate placement from INSIDE the window
// the org-member DELETE row names: the membership row has just been
// deleted, the second sweep has not run yet.
type windowPlacementGate struct {
	identity.Store
	s      *Server
	userID string

	mu      sync.Mutex
	placed  bool
	putCode int
	putBody string
	deletes int
}

func (g *windowPlacementGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if err := g.Store.DeleteOrgMembership(ctx, userID, orgID); err != nil {
		return err
	}
	if userID != g.userID {
		return nil
	}
	g.mu.Lock()
	g.deletes++
	first := !g.placed
	g.placed = true
	g.mu.Unlock()
	if !first {
		return nil
	}
	w := newErrorRecorder()
	g.s.handlePutOrgMember(w, teamReq(superAdminCtx(), "PUT", "/api/orgs/o1/members/"+g.userID, `{"role":"member"}`, "o1", g.userID))
	g.mu.Lock()
	g.putCode, g.putBody = w.Code, w.Body.String()
	g.mu.Unlock()
	return nil
}

// The last delete is blind to who wrote the row it takes, so a placement a
// super-admin issues INSIDE the removal's window answers 200 and leaves
// nothing behind. The off-boarding winning that race is the chosen
// doctrine — it is the one that keeps no access alive — but the placement's
// 200 says nothing about it, which is why the PUT row tells the caller to
// re-issue once the removal has returned. That re-issue is exercised here.
func TestOrgOffboarding_aPlacementInsideTheWindowIsTakenByTheLastDelete(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	gate := &windowPlacementGate{Store: real, s: s, userID: racerID}
	s.authSvc.SetStoreForTest(gate)

	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	s.authSvc.SetStoreForTest(real)
	if w.Code != http.StatusNoContent {
		t.Fatalf("off-boarding: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	gate.mu.Lock()
	putCode, putBody, deletes := gate.putCode, gate.putBody, gate.deletes
	gate.mu.Unlock()
	if putCode != http.StatusOK {
		t.Fatalf("the placement issued inside the window: code=%d body=%s, want 200 — then this is not the race the test describes", putCode, putBody)
	}
	if deletes != 2 {
		t.Fatalf("want 2 DeleteOrgMembership calls on the target, got %d — then the last delete did not run", deletes)
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the placement's row survived the removal (%+v) — then the last delete is not blind to who wrote it", om)
	}

	// The repair the PUT row names, on the state it left.
	again := newErrorRecorder()
	s.handlePutOrgMember(again, teamReq(superAdminCtx(), "PUT", "/api/orgs/o1/members/"+racerID, `{"role":"member"}`, "o1", racerID))
	if again.Code != http.StatusOK {
		t.Fatalf("the re-issued placement: code=%d body=%s, want 200", again.Code, again.Body.String())
	}
	if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
		t.Errorf("the re-issued placement left no row: %v", err)
	}
}

// off_ladder is a property of the grant's ROLE, which is known whether or
// not the team row can be read — so the console answers it on both
// branches. Assigning it beside orphan_grant left it false for a
// config_editor grant whose team row is gone, the branch the code itself
// calls the more alarming one: a consumer reading off_ladder as "this role
// needs no boundary" got the wrong answer there.
func TestAdminUserDetail_anOffLadderGrantWhoseTeamRowIsGoneStillSaysSo(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: racerID, Email: "racer@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "gone", Role: identity.RoleConfigEditor, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	u, err := s.authStore().GetUser(ctx, racerID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.buildAdminUserDetail(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	var gone, known *adminUserTeamView
	for i := range view.Teams {
		switch view.Teams[i].TeamID {
		case "gone":
			gone = &view.Teams[i]
		case "t1":
			known = &view.Teams[i]
		}
	}
	if gone == nil || known == nil {
		t.Fatalf("the console does not list both grants: %+v", view.Teams)
	}
	if !gone.MissingTeam {
		t.Errorf("the grant whose team row is gone is not flagged: %+v", *gone)
	}
	if !gone.OffLadder {
		t.Errorf("off_ladder is false for a %s grant whose team row is gone: %+v", gone.Role, *gone)
	}
	// The counterweight, on the branch that reads the team row: a LADDER
	// role is not off the ladder, or the flag would say nothing at all.
	if known.OffLadder {
		t.Errorf("off_ladder is set on a %s grant: %+v", known.Role, *known)
	}
}

// The removal acts on the account id the path gives it: it never requires
// the target to be a member and never 404s an unknown id, so a read it
// cannot answer must not turn an access-REMOVING endpoint into a refusal.
// An account that definitively has no row has no address an invitation
// could be addressed to, and no accept could turn one into access for THAT
// id — a registration mints a new account id (pkg/auth
// registerWithInvitation) and the existing-user accept logs in as this one —
// so the invitation half is skipped and the grants and the membership row
// are still taken. A store error that is NOT ErrNotFound still stops the
// cascade (TestOrgOffboarding_anUnreadableInvitationListStopsTheCascade),
// and what the skip leaves is listed by the org's own invitation route,
// asserted here rather than promised.
func TestOrgOffboarding_anAccountWithNoRowIsStillOffBoarded(t *testing.T) {
	s := newOrgCredsTestServer(t)
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, logs)
	ctx := context.Background()
	real := s.authStore()
	const ghost = "ghost"
	if err := real.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: ghost, OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: ghost, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	tok, invID := invitationFor(t, s, "t1", "ghost@example.com", identity.RoleMember)

	// Twice: the row the removal takes is the one the roster renders, and
	// the endpoint is documented idempotent — "re-issue it to finish the
	// sweep" is not a promise a permanent 500 can keep.
	for attempt := 1; attempt <= 2; attempt++ {
		w := newErrorRecorder()
		s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+ghost, "", "o1", ghost))
		if w.Code != http.StatusNoContent {
			t.Fatalf("attempt %d of the removal of an account with no row: code=%d body=%s, want 204",
				attempt, w.Code, w.Body.String())
		}
	}
	if om, err := real.GetOrgMembership(ctx, ghost, "o1"); err == nil {
		t.Errorf("the org membership survives a removal that answered 204: %+v", om)
	}
	if mb, err := real.GetMembership(ctx, ghost, "t1"); err == nil {
		t.Errorf("the team grant of an account with no row survives: %+v", mb)
	}
	// The residual the skip leaves, and the route that shows it: the
	// address is what the invitation half selects on, so an account with no
	// row leaves its invitations standing — nothing they can become is
	// access for THIS id, and the org admin sees them where they revoke
	// them by hand.
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Errorf("an invitation was revoked although no address could be read: %v", err)
	}
	list := newErrorRecorder()
	s.handleListOrgInvitations(list, orgReq(orgAdminCtx(), "GET", "/api/orgs/o1/invitations", "", "o1"))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), invID) {
		t.Errorf("the surviving invitation is not on the route the log line names: code=%d body=%s",
			list.Code, list.Body.String())
	}
	// And what makes leaving it safe for THIS id: the existing-user accept
	// reads the account first, so an id with no row cannot accept its way
	// back in — a registration that accepts the same row mints a new
	// account id instead (TestAcceptInvitation_claimsTheInvitationBeforeAnyAccess
	// records the CreateUser).
	if mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, ghost, tok); err == nil {
		t.Errorf("the surviving invitation was accepted into the id that has no account row: %+v", mb)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("the accept of an id with no account row: %v, want identity.ErrNotFound", err)
	}
	logged := logs.String()
	for _, want := range []string{"o1", ghost, "no account row", "invitations"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the skipped invitation half is not named where it survives the caller (missing %q): %s", want, logged)
		}
	}
}

// grantListErrorGate fails the grant listing of one user: the first pass's
// own failure point, immediately past the invitation half that a missing
// account row makes it skip.
type grantListErrorGate struct {
	identity.Store
	userID string
}

func (g *grantListErrorGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	if userID != g.userID {
		return g.Store.ListMembershipsByUser(ctx, userID)
	}
	return nil, errors.New("identity: list memberships by user: connection reset by peer")
}

// The line that reports the skipped invitation half says what THIS read
// found — no address to select the pending invitations by, so they are left
// standing on the route that lists them. It is emitted before the grant
// sweep and before any delete of the membership row, so a claim about what
// the removal revoked would be a claim about a state the request may never
// reach: here the pass fails one step later and answers that the org
// membership STANDS, with the team grant standing too. It is the only
// surface that reports this residual, and the deployed level emits it
// (charts/iterion/values.yaml config.log.level: info), so an operator
// greps it for off-boardings to finish by hand.
func TestOrgOffboarding_theSkippedInvitationHalfIsLoggedAsFoundNotAsFinished(t *testing.T) {
	s := newOrgCredsTestServer(t)
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, logs)
	ctx := context.Background()
	real := s.authStore()
	const ghost = "ghost"
	if err := real.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: ghost, OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: ghost, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.authSvc.SetStoreForTest(&grantListErrorGate{Store: real, userID: ghost})
	w := newErrorRecorder()
	s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+ghost, "", "o1", ghost))
	s.authSvc.SetStoreForTest(real)

	body := w.Diagnostic(t)
	if w.Code != http.StatusInternalServerError || !strings.Contains(body, "their org membership stands") {
		t.Fatalf("not the exit this test is about: code=%d body=%s, want 500 from the first pass", w.Code, body)
	}
	if _, err := real.GetOrgMembership(ctx, ghost, "o1"); err != nil {
		t.Fatalf("the membership the answer says STANDS is gone: %v", err)
	}
	if _, err := real.GetMembership(ctx, ghost, "t1"); err != nil {
		t.Fatalf("the grant is gone although the pass failed before the grant sweep: %v", err)
	}
	logged := logs.String()
	for _, want := range []string{"no account row", "GET /api/orgs/o1/invitations"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the skip is not reported where the operator greps it (missing %q): %s", want, logged)
		}
	}
	// What the line must never carry: the removal's outcome. The answer and
	// the state above are the witnesses that it has not happened.
	for _, claimed := range []string{"are revoked", "is revoked", "were revoked", "have been revoked"} {
		if strings.Contains(logged, claimed) {
			t.Errorf("the log asserts %q while the answer says the opposite and both rows stand\n  answer: %s\n  log:    %s",
				claimed, strings.TrimSpace(body), strings.TrimSpace(logged))
		}
	}
}

// secondSweepFailureGate puts the handler on the exit where the SECOND sweep
// fails, with a legitimate placement landing inside the window — and,
// optionally, a last delete that fails too.
type secondSweepFailureGate struct {
	identity.Store
	s              *Server
	userID         string
	failLastDelete bool

	mu      sync.Mutex
	deletes int
	placed  bool
	putCode int
	putBody string
}

func (g *secondSweepFailureGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if userID != g.userID {
		return g.Store.DeleteOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	g.deletes++
	n := g.deletes
	g.mu.Unlock()
	if n > 1 && g.failLastDelete {
		return errors.New("identity: delete org membership: connection reset by peer")
	}
	if err := g.Store.DeleteOrgMembership(ctx, userID, orgID); err != nil {
		return err
	}
	if n > 1 {
		return nil
	}
	w := newErrorRecorder()
	g.s.handlePutOrgMember(w, teamReq(superAdminCtx(), "PUT", "/api/orgs/o1/members/"+g.userID, `{"role":"member"}`, "o1", g.userID))
	g.mu.Lock()
	g.putCode, g.putBody = w.Code, w.Body.String()
	g.placed = true
	g.mu.Unlock()
	return nil
}

// Every read of t1's invitations ONCE the placement has landed fails — the
// second sweep's, whichever number of reads the window's own placement made
// (it reconciles the invitations its subject minted).
func (g *secondSweepFailureGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	if teamID != "t1" {
		return g.Store.ListInvitationsByTeam(ctx, teamID)
	}
	g.mu.Lock()
	placed := g.placed
	g.mu.Unlock()
	if placed {
		return nil, errors.New("identity: list invitations by team: connection reset by peer")
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

// A second sweep that fails is not a reason to leave the boundary row: that
// row is the one residual this handler can still take, and the state it
// leaves is the most invisible of the four — the console carries no drift
// flag on a grant whose org membership is back. So the last delete runs
// whether or not the sweep before it succeeded, which is what makes the
// exit's own "was removed" true; when that delete fails too, one body names
// both halves.
func TestOrgOffboarding_aFailedSecondSweepStillTakesTheBoundaryRow(t *testing.T) {
	ctx := context.Background()

	t.Run("the boundary row written inside the window is still taken", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedRacer(t, s)
		real := s.authStore()
		gate := &secondSweepFailureGate{Store: real, s: s, userID: racerID}
		s.authSvc.SetStoreForTest(gate)

		w := newErrorRecorder()
		s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
		s.authSvc.SetStoreForTest(real)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("off-boarding whose second sweep failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
		}
		gate.mu.Lock()
		putCode, putBody, deletes := gate.putCode, gate.putBody, gate.deletes
		gate.mu.Unlock()
		if putCode != http.StatusOK {
			t.Fatalf("the placement issued inside the window: code=%d body=%s, want 200 — then this is not the race the test describes", putCode, putBody)
		}
		if body := w.Diagnostic(t); !strings.Contains(body, "may still hold a team grant in this org") {
			t.Errorf("the 500 does not name what the failed sweep may have left: %s", body)
		}
		// The invitation residue this body carries is the LAST sweep's own
		// clause, which failed here too — never the grant pass's words.
		if body := w.Diagnostic(t); strings.Contains(body, standingMintedInvitation) &&
			!strings.Contains(body, "the last sweep of their pending invitations failed") {
			t.Errorf("the 500 names an invitation residue outside the last sweep's own clause: %s", body)
		}
		if deletes != 2 {
			t.Fatalf("want 2 DeleteOrgMembership calls on the target, got %d — then the last delete did not run on this exit", deletes)
		}
		// The state the body claims, asserted on the store and on the
		// access it buys: "was removed" of a member who can read the
		// roster is the sentence this exit used to ship.
		if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
			t.Errorf("the membership written inside the window stands although the body says the member was removed: %+v", om)
		}
		roster := newErrorRecorder()
		s.handleListOrgMembers(roster, orgReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: racerID}), "GET", "/api/orgs/o1/members", "", "o1"))
		if roster.Code != http.StatusForbidden {
			t.Errorf("the removed member reads the org roster: code=%d body=%s, want 403", roster.Code, roster.Diagnostic(t))
		}
	})

	t.Run("and when that delete fails too, one body names both halves", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedRacer(t, s)
		logs := &lockedBuffer{}
		s.logger = iterlog.New(iterlog.LevelError, logs)
		real := s.authStore()
		gate := &secondSweepFailureGate{Store: real, s: s, userID: racerID, failLastDelete: true}
		s.authSvc.SetStoreForTest(gate)

		w := newErrorRecorder()
		s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
		s.authSvc.SetStoreForTest(real)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("off-boarding whose second sweep and last delete both failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
		}
		body := w.Diagnostic(t)
		for _, want := range []string{
			"re-sweeping their team access failed",
			"re-deleting their org membership failed too",
			"may still hold a team grant in this org",
			"they would be an org member again",
			"re-issue the removal",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the 500 of both failures does not name what it left (missing %q): %s", want, body)
			}
		}
		// And the row it names IS standing — the placement's, which the
		// delete could not take.
		if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
			t.Errorf("the membership the body says may stand is gone: %v", err)
		}
		logged := logs.String()
		for _, want := range []string{racerID, "o1", "may stand"} {
			if !strings.Contains(logged, want) {
				t.Errorf("the log line does not name what stands (missing %q): %s", want, logged)
			}
		}
	})
}

// The two exits that report a FINISHED first pass carry the condition that
// pass has: an invitation names an ADDRESS, so a target whose account row
// cannot be read has none to select by and the invitation half is skipped
// (revokeOrgTeamAccess). Both bodies are read by the operator the answer
// sends back for a re-issue, and the re-issue is exercised here on the state
// they describe: it answers 204 and the invitation is STILL standing, since
// nothing about it changed — which is why neither body may promise it was
// taken.
func TestOrgOffboarding_theBodiesThatReportAFinishedFirstPassCarryItsCondition(t *testing.T) {
	for _, c := range []struct {
		name       string
		failOnCall int
		says       string
		forbidden  string
	}{
		{
			name:       "the membership row's own delete",
			failOnCall: 1,
			says:       "their team grants in this org are revoked, with the pending invitations they minted here that carry an org role (and those addressed to them, where their account row named an address)",
			forbidden:  "their team grants and pending invitations in this org are revoked",
		},
		{
			name:       "the last delete, both sweeps through",
			failOnCall: 2,
			says:       "are revoked, with the pending invitations they minted here that carry an org role (and those addressed to them, where their account row named an address)",
			forbidden:  "the team access of ghost in org o1 was revoked",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			ctx := context.Background()
			real := s.authStore()
			const ghost = "ghost"
			if err := real.UpsertOrgMembership(ctx, identity.OrgMembership{
				UserID: ghost, OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := real.UpsertMembership(ctx, identity.Membership{
				UserID: ghost, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			_, invID := invitationFor(t, s, "t1", "ghost@example.com", identity.RoleMember)
			s.authSvc.SetStoreForTest(&orgMembershipDeleteGate{Store: real, userID: ghost, orgID: "o1", failOnCall: c.failOnCall})

			w := newErrorRecorder()
			s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+ghost, "", "o1", ghost))
			body := w.Diagnostic(t)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("off-boarding of an account with no row: code=%d body=%s, want 500", w.Code, body)
			}
			// The row first: a body is not a witness, and a change that
			// revoked the invitation anyway must not leave this green.
			inv, err := real.GetInvitation(ctx, invID)
			if err != nil {
				t.Fatalf("the invitation was revoked although no address could be read (%v) — then this is not the state the body describes", err)
			}
			if inv.AcceptedAt != nil {
				t.Fatalf("the standing invitation is not pending: %+v", inv)
			}
			if !strings.Contains(body, c.says) {
				t.Errorf("the 500 does not carry the first pass's own condition (missing %q): %s", c.says, body)
			}
			if strings.Contains(body, c.forbidden) {
				t.Errorf("the 500 says %q while invitation %s stands: %s", c.forbidden, inv.ID, body)
			}
			// The repair the body asks for, on the state it left: the
			// removal finishes, and the invitation is still there to be
			// revoked by hand.
			s.authSvc.SetStoreForTest(real)
			again := newErrorRecorder()
			s.handleRemoveOrgMember(again, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+ghost, "", "o1", ghost))
			if again.Code != http.StatusNoContent {
				t.Fatalf("the re-issue this body asks for: code=%d body=%s, want 204", again.Code, again.Diagnostic(t))
			}
			if _, err := real.GetInvitation(ctx, invID); err != nil {
				t.Errorf("the re-issue took the invitation (%v) — then the condition the bodies carry is not needed", err)
			}
		})
	}
}

// The off-boarding's two REQUEST-context exits say what they left where it
// survives the caller. A client that goes away is the ordinary way they are
// reached — it is the reason every detached context in this handler exists —
// and these two are the exits no audit row covers either: org_member.removed
// is written once the membership row is gone. The four other exits already
// log (the sweepErr and lastErr lines of the detached tail, and the grant
// guard's own).
func TestOrgOffboarding_theRequestContextExitsLogThePartialOffBoarding(t *testing.T) {
	t.Run("the first pass fails", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		logs := &lockedBuffer{}
		s.logger = iterlog.New(iterlog.LevelError, logs)
		counted := &countingAudit{Store: audit.NewMemoryStore()}
		s.auditStore = counted
		seedRacer(t, s)
		ctx := context.Background()
		real := s.authStore()
		s.authSvc.SetStoreForTest(&grantListErrorGate{Store: real, userID: racerID})

		w := newErrorRecorder()
		s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
		s.authSvc.SetStoreForTest(real)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("off-boarding whose first pass failed: code=%d body=%s, want 500", w.Code, w.Body.String())
		}
		if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
			t.Fatalf("the membership the answer says STANDS is gone: %v", err)
		}
		logged := logs.String()
		for _, want := range []string{"o1", racerID, "connection reset by peer", "their org membership stands"} {
			if !strings.Contains(logged, want) {
				t.Errorf("the log does not name the partial off-boarding (missing %q): %s", want, logged)
			}
		}
		// And no audit row: org_member.removed is written once the
		// membership row is gone, so the log is this exit's only trail.
		awaitAuditQuiet(t, counted, 0)
		if rows := auditRows(t, s, "o1", racerID); len(rows) != 0 {
			t.Errorf("the failed first pass wrote org audit rows %v — then the log is not this exit's only trail", auditActionsOf(rows))
		}
	})
	t.Run("the membership row's own delete fails", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		logs := &lockedBuffer{}
		s.logger = iterlog.New(iterlog.LevelError, logs)
		counted := &countingAudit{Store: audit.NewMemoryStore()}
		s.auditStore = counted
		seedRacer(t, s)
		ctx := context.Background()
		real := s.authStore()
		if err := real.UpsertMembership(ctx, identity.Membership{
			UserID: racerID, TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		s.authSvc.SetStoreForTest(&orgMembershipDeleteGate{Store: real, userID: racerID, orgID: "o1", failOnCall: 1})

		w := newErrorRecorder()
		s.handleRemoveOrgMember(w, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
		s.authSvc.SetStoreForTest(real)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("off-boarding whose membership delete failed: code=%d body=%s, want 500", w.Code, w.Body.String())
		}
		if _, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil {
			t.Fatalf("the membership went although its delete failed — not the state this test is about: %v", err)
		}
		if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
			t.Fatalf("the first sweep left grant %+v — not the state this test is about", mb)
		}
		logged := logs.String()
		for _, want := range []string{"o1", racerID, "connection reset by peer", "may or may not be gone"} {
			if !strings.Contains(logged, want) {
				t.Errorf("the log does not name the state the 500 left (missing %q): %s", want, logged)
			}
		}
		// The first pass's own revoke of the t1 grant is the ONE row this
		// cascade accounts for; the org-scoped trail stays empty, so the log
		// is again the only account of what the 500 left.
		awaitAuditQuiet(t, counted, 1)
		if rows := auditRows(t, s, "o1", racerID); len(rows) != 0 {
			t.Errorf("the off-boarding that failed on the membership row wrote org audit rows %v — org_member.removed is written once that row is gone", auditActionsOf(rows))
		}
	})
}
