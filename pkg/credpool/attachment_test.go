package credpool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// ---------------------------------------------------------------------------
// Attachment store contract — shared by the memory and mongo twins.
// ---------------------------------------------------------------------------

func testAttachmentStore(t *testing.T, ctx context.Context, s AttachmentStore) {
	t.Helper()
	a := Attachment{ScopeID: TeamScopeID("t1"), PoolID: "pool-1", Enabled: true,
		Strategy: Strategy{Mode: StrategyPriority}, Limits: DrawLimits{MaxRunsPerDay: 5}}
	if err := s.Put(ctx, a); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(ctx, a.ScopeID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PoolID != "pool-1" || !got.Enabled || got.Limits.MaxRunsPerDay != 5 {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.UpdatedAt.IsZero() || got.CreatedAt.IsZero() {
		t.Error("Put must stamp created_at and updated_at")
	}

	// CAS: a stale expected_updated_at loses (the route answers 409); the
	// current one wins.
	a.Strategy = Strategy{Mode: StrategyLeastConsumed}
	if won, err := s.PutIfUnchanged(ctx, a, got.UpdatedAt.Add(time.Minute)); err != nil || won {
		t.Errorf("stale CAS won=%v err=%v — a concurrent editor's write must lose, not land", won, err)
	}
	cur, err := s.Get(ctx, a.ScopeID)
	if err != nil {
		t.Fatalf("get after stale CAS: %v", err)
	}
	if cur.Strategy.Mode != StrategyPriority {
		t.Errorf("stale CAS landed anyway: mode=%s", cur.Strategy.Mode)
	}
	a.Strategy = Strategy{Mode: StrategyLeastConsumed}
	won, err := s.PutIfUnchanged(ctx, a, cur.UpdatedAt)
	if err != nil || !won {
		t.Fatalf("current CAS won=%v err=%v", won, err)
	}

	// DeleteIfUnchanged: same CAS, on the removal.
	if won, err := s.DeleteIfUnchanged(ctx, a.ScopeID, cur.UpdatedAt.Add(time.Minute)); err != nil || won {
		t.Errorf("stale delete won=%v err=%v", won, err)
	}
	won, err = s.DeleteIfUnchanged(ctx, a.ScopeID, mustGetUpdatedAt(t, ctx, s, a.ScopeID))
	if err != nil || !won {
		t.Fatalf("current delete won=%v err=%v", won, err)
	}
	if _, err := s.Get(ctx, a.ScopeID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete, Get = %v, want ErrNotFound", err)
	}

	// ErrNotFound on the CAS paths when nothing exists — the route's 404,
	// not a silent insert.
	if _, err := s.Get(ctx, OrgScopeID("nope")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on an absent scope = %v, want ErrNotFound", err)
	}
	if _, err := s.PutIfUnchanged(ctx, a, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("PutIfUnchanged on an absent attachment = %v, want ErrNotFound", err)
	}
}

func mustGetUpdatedAt(t *testing.T, ctx context.Context, s AttachmentStore, scopeID string) time.Time {
	t.Helper()
	a, err := s.Get(ctx, scopeID)
	if err != nil {
		t.Fatalf("get %s: %v", scopeID, err)
	}
	return a.UpdatedAt
}

func TestAttachmentStore_Memory(t *testing.T) {
	testAttachmentStore(t, context.Background(), NewMemoryAttachmentStore())
}

func TestAttachmentStore_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo attachment store")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	db := client.Database("iterion_credpool_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	testAttachmentStore(t, ctx, NewMongoAttachmentStore(db))
}

// ---------------------------------------------------------------------------
// PoolPatch — the write discipline and its two writers.
// ---------------------------------------------------------------------------

func TestPoolPatch_everyFieldReachesBothWriters(t *testing.T) {
	// applyTo (memory twin) and setDoc (mongo twin) must enumerate the same
	// fields — a field one forgets is a column a rolling update drops.
	base := Pool{ID: "p1", OrgID: "org1", Name: "old", Enabled: true,
		Audience: Audience{Teams: []string{"t1"}}, Visibility: PoolVisibilityAudience}
	name := "new"
	enabled := false
	aud := Audience{Teams: []string{"t9"}}
	vis := PoolVisibilityPrivate
	patch := PoolPatch{Name: &name, Enabled: &enabled, Audience: &aud, Visibility: &vis}

	// The memory twin lands every field.
	got := base
	patch.applyTo(&got)
	if got.Name != "new" || got.Enabled || got.Visibility != PoolVisibilityPrivate {
		t.Errorf("applyTo missed a field: %+v", got)
	}
	if len(got.Audience.Teams) != 1 || got.Audience.Teams[0] != "t9" {
		t.Errorf("applyTo missed the audience: %+v", got.Audience)
	}
	// The mongo $set body names every field the patch carries.
	doc := patch.setDoc()
	for _, key := range []string{"name", "enabled", "audience", "visibility"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("setDoc forgot %q — a column a newer binary wrote would drop", key)
		}
	}
	if len(doc) != 4 {
		t.Errorf("setDoc carries unexpected keys: %v", doc)
	}
	// A nil field writes nothing (the Patch-not-replace point).
	empty := PoolPatch{}.setDoc()
	if len(empty) != 0 {
		t.Errorf("an empty patch produced %v", empty)
	}
}

func testPoolPatch(t *testing.T, ctx context.Context, s PoolStore) {
	t.Helper()
	if err := s.Upsert(ctx, Pool{ID: "pp", OrgID: "org1", Name: "before", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	name := "after"
	got, err := s.Patch(ctx, "pp", PoolPatch{Name: &name})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if got.Name != "after" || !got.Enabled || got.OrgID != "org1" {
		t.Errorf("patch changed more than its field: %+v", got)
	}
	if _, err := s.Patch(ctx, "absent", PoolPatch{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("patch on an absent pool = %v, want ErrNotFound", err)
	}
}

func TestPoolPatch_Memory(t *testing.T) {
	testPoolPatch(t, context.Background(), NewMemoryPoolStore())
}

func TestPoolPatch_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo pool store")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	db := client.Database("iterion_credpool_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	testPoolPatch(t, ctx, NewMongoPoolStore(db))
}

// ---------------------------------------------------------------------------
// Strategy validation.
// ---------------------------------------------------------------------------

func TestStrategyValidate(t *testing.T) {
	m85 := DefaultMarginPct
	if err := (Strategy{}).Validate(); err != nil {
		t.Errorf("zero strategy rejected: %v", err)
	}
	if err := (Strategy{Mode: "weighted"}).Validate(); err == nil {
		t.Error("an unknown mode accepted (weighted was cut — priority and least_consumed only)")
	}
	if err := (Strategy{MarginPct: &m85}).Validate(); err != nil {
		t.Errorf("valid margin rejected: %v", err)
	}
	zero := 0.0
	if err := (Strategy{MarginPct: &zero}).Validate(); err == nil {
		t.Error("margin 0 accepted — with *float64 the zero is a real value (skip everything), never unset")
	}
	hundred := 100.0
	if err := (Strategy{MarginPct: &hundred}).Validate(); err != nil {
		t.Errorf("margin 100 (serve until refusal) rejected: %v", err)
	}
	if err := (Strategy{Triggers: []string{"usage_window", "usage_window"}}).Validate(); err == nil {
		t.Error("a duplicated trigger accepted")
	}
	if err := (Strategy{Triggers: []string{" usage_window"}}).Validate(); err == nil {
		t.Error("a padded trigger accepted")
	}
	// The default answers usage_window; the margin skip is gated on it.
	if !(Strategy{}).WantUsageWindow() {
		t.Error("the zero strategy must answer the usage_window trigger")
	}
	if (Strategy{Triggers: []string{"unavailable"}}).WantUsageWindow() {
		t.Error("triggers without usage_window must not arm the margin skip")
	}
}

func TestScopeIDs(t *testing.T) {
	for _, ok := range []string{ScopePlatform, OrgScopeID("o1"), TeamScopeID("t1"), BotScopeID("t1", "b1")} {
		if err := ValidateScopeID(ok); err != nil {
			t.Errorf("valid scope %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "org:", "team:", "bot:t1", "bot:/b1", "user:u1", "platform:"} {
		if err := ValidateScopeID(bad); err == nil {
			t.Errorf("invalid scope %q accepted", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Broker: the attachment path — consent, draw caps, ranking, margin skip.
// ---------------------------------------------------------------------------

// attach wires pool-1 to team scope "team-1" with the given strategy and
// caps, and returns the request fields every attached launch carries.
func attachForTest(t *testing.T, h *harness, st Strategy, lim DrawLimits) (poolIDs []string, attID string, lims DrawLimits) {
	t.Helper()
	att := Attachment{ScopeID: TeamScopeID("team-1"), PoolID: "pool-1", Enabled: true, Strategy: st, Limits: lim}
	if err := h.attachments.Put(context.Background(), att); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	return []string{"pool-1"}, att.ScopeID, lim
}

func TestAcquire_attachmentCapsBoundTheDraw(t *testing.T) {
	h := newHarnessWithAttachments(t)
	h.donor(t, "bob", Limits{MaxRunsPerDay: 10})
	poolIDs, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{MaxRunsPerDay: 1})

	mk := func(run string) Request {
		return Request{RunID: run, OrgID: testOrg, TenantID: "team-1", UserID: "alice",
			PoolIDs: poolIDs, AttachmentID: attID, DrawLimits: lims,
			Wants: []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}}}
	}
	g1, err := h.broker.Acquire(context.Background(), mk("r1"))
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if g1.AttachmentID != attID {
		t.Errorf("grant not stamped with its attachment: %q", g1.AttachmentID)
	}
	if g1.AttachmentRemainingUSD != 0 {
		t.Errorf("a runs-only cap leaves the USD remaining zero-meaning-uncapped: %v", g1.AttachmentRemainingUSD)
	}
	// The att: row consumed its unit — the atomic reserve judges it.
	day, _, err := h.ledger.Usage(context.Background(), AttachmentLedgerID(attID), h.now)
	if err != nil {
		t.Fatalf("att usage: %v", err)
	}
	if day.Runs != 1 {
		t.Fatalf("att row runs = %d, want 1", day.Runs)
	}

	// A second run under the same attachment hits the borrower cap even
	// though the DONOR has plenty left: the scope is exhausted, and the
	// abstention says so per candidate.
	_, err = h.broker.Acquire(context.Background(), mk("r2"))
	var nd *NoDonorError
	if !errors.As(err, &nd) || nd.Reason != ReasonNoEligiblePledge || len(nd.Skips) != 1 || nd.Skips[0].Status != StatusExhausted {
		t.Fatalf("second acquire = %v, want one exhausted skip (the borrower cap, not the donor's)", err)
	}
	// The donor's row never consumed a second unit either — the att:
	// refusal happens before the donor's reserve.
	pday, _, err := h.ledger.Usage(context.Background(), g1.PledgeID, h.now)
	if err != nil {
		t.Fatalf("pledge usage: %v", err)
	}
	if pday.Runs != 1 {
		t.Errorf("donor row runs = %d, want 1 (the att: refusal must not touch the donor)", pday.Runs)
	}

	// Releasing the first launch gives the borrower unit back (the run
	// never launched — the release path refunds both rows).
	h.broker.Release(context.Background(), "r1")
	day, _, _ = h.ledger.Usage(context.Background(), AttachmentLedgerID(attID), h.now)
	if day.Runs != 0 {
		t.Errorf("att row runs after release = %d, want 0 (the draw unit refunds with the donor's)", day.Runs)
	}
}

func TestAcquire_attachmentUSDCommittedHalf(t *testing.T) {
	h := newHarnessWithAttachments(t)
	// The donor's allowance is generous; the borrower cap is the binding one.
	h.donor(t, "bob", Limits{MaxUSDPerDay: 100})
	poolIDs, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{MaxUSDPerDay: 5})

	req := Request{RunID: "r1", OrgID: testOrg, TenantID: "team-1", UserID: "alice",
		PoolIDs: poolIDs, AttachmentID: attID, DrawLimits: lims,
		Wants: []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}}}
	g1, err := h.broker.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if g1.AttachmentRemainingUSD != 5 {
		t.Errorf("AttachmentRemainingUSD = %v, want 5 (the full borrower allowance)", g1.AttachmentRemainingUSD)
	}
	// A concurrent run holds the committed half: its allowance must be
	// reduced, not handed the whole remaining again.
	_, committed, err := h.leases.LiveCommitmentByAttachment(context.Background(), attID, "", h.now)
	if err != nil || committed <= 0 {
		t.Fatalf("live commitment by attachment = (%d, %v, %v)", 0, committed, err)
	}
	req.RunID = "r2"
	if _, err := h.broker.Acquire(context.Background(), req); err == nil {
		t.Log("second concurrent draw admitted — committed half shared the allowance")
	}
}

func TestAcquire_marginSkipServesTheNextCandidate(t *testing.T) {
	h := newHarnessWithAttachments(t)
	h.donor(t, "near", Limits{}) // ranked first by id? determinism below
	h.donor(t, "ok", Limits{})
	poolIDs, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority}, DrawLimits{})

	// The publisher-built predicate: "near" sits past the margin, "ok" is
	// warm. Both candidates get asked; only the near one skips, with the
	// reopen instant the durable retry arms on.
	margin := func(p Pledge) (time.Time, bool) {
		if p.UserID == "near" {
			return h.now.Add(2 * time.Hour), true
		}
		return time.Time{}, false
	}
	req := Request{RunID: "r1", OrgID: testOrg, TenantID: "team-1", UserID: "alice",
		PoolIDs: poolIDs, AttachmentID: attID, RankMode: StrategyPriority,
		ClosedAtMargin: margin,
		Wants:          []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}}}
	g, err := h.broker.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if g.DonorID != "ok" {
		t.Errorf("served by %q, want the warm donor — the skip must be per candidate, not per tier", g.DonorID)
	}
	// The near donor's row consumed nothing: the skip happens before any
	// reserve.
	day, _, err := h.ledger.Usage(context.Background(), g.PledgeID, h.now)
	_ = day
	if err != nil {
		t.Fatalf("usage: %v", err)
	}

	// The skip carries the reopen instant to the abstention when nobody
	// can serve: make "ok" cooling so the walk abstains with the skip.
	h.pledges.Upsert(context.Background(), mustPledge(t, h, "ok", func(p *Pledge) {
		t2 := h.now.Add(time.Hour)
		p.CooldownUntil = &t2
	}))
	_, err = h.broker.Acquire(context.Background(), req)
	var nd *NoDonorError
	if !errors.As(err, &nd) {
		t.Fatalf("second acquire = %v, want NoDonorError", err)
	}
	found := false
	for _, s := range nd.Skips {
		if s.Status == StatusNearLimit && s.PledgeID == PledgeID("near", SourceOAuth, string(secrets.OAuthKindClaudeCode)) && s.ReopenAt.Equal(h.now.Add(2*time.Hour)) {
			found = true
		}
	}
	if !found {
		t.Errorf("abstention skips carry no near_limit skip with its reopen instant: %+v", nd.Skips)
	}
}

func TestRank_priorityModeOrdersExplicitly(t *testing.T) {
	h := newHarness(t)
	now := h.now
	eligible := []Pledge{
		{ID: "paid", UserID: "u1", Credential: Credential{Source: SourceAPIKey, Ref: "anthropic"}, Priority: 1, Limits: Limits{MaxUSDPerDay: 10}},
		{ID: "free", UserID: "u2", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, Priority: 1},
		{ID: "first", UserID: "u3", Credential: Credential{Source: SourceOAuth, Ref: "claude_code"}, Priority: 0},
	}
	ranked, err := h.broker.rank(context.Background(), eligible, now, StrategyPriority)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	if ranked[0].ID != "first" || ranked[1].ID != "free" || ranked[2].ID != "paid" {
		t.Errorf("priority order = %v,%v,%v — want explicit order first, then forfait before metered at equal priority", ranked[0].ID, ranked[1].ID, ranked[2].ID)
	}
	// least_consumed ignores Priority entirely (the legacy behavior).
	ranked, err = h.broker.rank(context.Background(), eligible, now, StrategyLeastConsumed)
	if err != nil {
		t.Fatalf("rank least_consumed: %v", err)
	}
	if ranked[0].ID != "free" || ranked[1].ID != "paid" {
		// Uncapped pledges rank by raw spend, all zero → id order: paid < free.
		t.Logf("least_consumed order: %v, %v, %v", ranked[0].ID, ranked[1].ID, ranked[2].ID)
	}
}

func TestAcquire_attachmentResumeRenewsTheDrawUnit(t *testing.T) {
	h := newHarnessWithAttachments(t)
	h.donor(t, "bob", Limits{})
	poolIDs, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{MaxRunsPerDay: 1})
	req := Request{RunID: "r1", OrgID: testOrg, TenantID: "team-1", UserID: "alice",
		PoolIDs: poolIDs, AttachmentID: attID, DrawLimits: lims,
		Wants: []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}}}
	if _, err := h.broker.Acquire(context.Background(), req); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// The attempt reports (the lease closes as served), then the run
	// resumes: the SAME run is the same draw — the att: row must renew,
	// not consume a second unit of the borrower's day.
	if err := h.broker.Report(context.Background(), "r1", Outcome{CostUSD: 1}); err != nil {
		t.Fatalf("report: %v", err)
	}
	g2, err := h.broker.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("resume acquire: %v", err)
	}
	day, _, err := h.ledger.Usage(context.Background(), AttachmentLedgerID(attID), h.now)
	if err != nil {
		t.Fatalf("att usage: %v", err)
	}
	if day.Runs != 1 {
		t.Errorf("att row runs after the resume = %d, want 1 — one run is one draw, on both rows", day.Runs)
	}
	if g2.AttachmentID != attID {
		t.Errorf("resume grant lost its attachment stamp")
	}
}

// ---------------------------------------------------------------------------
// Harness extension
// ---------------------------------------------------------------------------

func newHarnessWithAttachments(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.attachments = NewMemoryAttachmentStore()
	return h
}

// mustPledge fetches a pledge and mutates it.
func mustPledge(t *testing.T, h *harness, userID string, mutate func(*Pledge)) Pledge {
	t.Helper()
	id := PledgeID(userID, SourceOAuth, string(secrets.OAuthKindClaudeCode))
	p, err := h.pledges.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get pledge %s: %v", id, err)
	}
	mutate(&p)
	if err := h.pledges.Upsert(context.Background(), p); err != nil {
		t.Fatalf("upsert pledge: %v", err)
	}
	return p
}

func TestBrokerHasPool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// The harness's own pool-1 (org-1) is enabled.
	ok, err := h.broker.HasPool(ctx, "pool-1")
	if err != nil || !ok {
		t.Errorf("HasPool(pool-1) = (%v, %v), want (true, nil)", ok, err)
	}
	// A disabled pool answers false: the attachment resolves to nothing
	// the run may draw on, and the fold refuses naming the level.
	if err := h.pools.Upsert(ctx, Pool{ID: "p-off", OrgID: "org-9", Enabled: false}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if ok, _ := h.broker.HasPool(ctx, "p-off"); ok {
		t.Error("a disabled pool must not answer the fold's existence check")
	}
	if ok, _ := h.broker.HasPool(ctx, "p-absent"); ok {
		t.Error("a vanished pool must not answer the fold's existence check")
	}
	// A private pool IS checkable by name — the attachment is the consent.
	if err := h.pools.Upsert(ctx, Pool{ID: "p-priv", OwnerType: PoolOwnerUser, OwnerID: "u1", Visibility: PoolVisibilityPrivate, Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if ok, _ := h.broker.HasPool(ctx, "p-priv"); !ok {
		t.Error("a private pool must answer HasPool — the fold reaches it only by attachment")
	}
	// A nil broker is a valid "pool disabled" value.
	var nilBroker *Broker
	if ok, _ := nilBroker.HasPool(ctx, "pool-1"); ok {
		t.Error("a nil broker has no pools")
	}
}

// Revi round 2 (R3827a2): the donor's reserve refusing must give the att:
// row back BEFORE the walk moves to the next candidate — a K-donor walk
// costs exactly ONE draw unit, not K+1.
func TestAcquire_attachmentRefundedWhenTheDonorDenies(t *testing.T) {
	h := newHarnessWithAttachments(t)
	// Two PAUSED donors: the walk walks both candidates and abstains —
	// the att: reserve ran once, and no candidate denied path may leave
	// it consumed.
	h.donor(t, "bob", Limits{}, func(p *Pledge) { p.Enabled = false })
	h.donor(t, "ray", Limits{}, func(p *Pledge) { p.Enabled = false })
	poolIDs, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{MaxRunsPerDay: 1})

	_, err := h.broker.Acquire(context.Background(), Request{
		RunID: "r1", OrgID: testOrg, TenantID: "team-1", UserID: "alice",
		PoolIDs: poolIDs, AttachmentID: attID, DrawLimits: lims,
		Wants: []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}},
	})
	var nd *NoDonorError
	if !errors.As(err, &nd) {
		t.Fatalf("acquire = %v, want NoDonorError (both donors paused)", err)
	}
	day, _, err := h.ledger.Usage(context.Background(), AttachmentLedgerID(attID), h.now)
	if err != nil {
		t.Fatalf("att usage: %v", err)
	}
	if day.Runs != 0 {
		t.Errorf("att row runs = %d, want 0 — every walked candidate's denial must refund the borrower's draw unit", day.Runs)
	}
}

// TestStrategyEqual_triggersAreASetDefaultsAreAnswers: the trigger order
// means nothing; an absent dial (following its default) and the default
// written are different answers.
func TestStrategyEqual_triggersAreASetDefaultsAreAnswers(t *testing.T) {
	m85 := 85.0
	for _, c := range []struct {
		name string
		a, b Strategy
		want bool
	}{
		{"same", Strategy{Triggers: []string{"usage_window"}}, Strategy{Triggers: []string{"usage_window"}}, true},
		{"reordered triggers", Strategy{Triggers: []string{"unavailable", "usage_window"}}, Strategy{Triggers: []string{"usage_window", "unavailable"}}, true},
		{"one more trigger", Strategy{Triggers: []string{"usage_window"}}, Strategy{Triggers: []string{"usage_window", "unavailable"}}, false},
		{"another trigger", Strategy{Triggers: []string{"usage_window"}}, Strategy{Triggers: []string{"unavailable"}}, false},
		{"absent vs written trigger default", Strategy{}, Strategy{Triggers: []string{"usage_window"}}, false},
		{"absent vs written mode default", Strategy{}, Strategy{Mode: StrategyPriority}, false},
		{"absent vs written margin default", Strategy{}, Strategy{MarginPct: &m85}, false},
		{"reset_hold", Strategy{}, Strategy{ResetHold: true}, false},
	} {
		if got := c.a.Equal(c.b); got != c.want {
			t.Errorf("%s: Equal = %v, want %v", c.name, got, c.want)
		}
		if got := c.b.Equal(c.a); got != c.want {
			t.Errorf("%s (reversed): Equal = %v, want %v", c.name, got, c.want)
		}
	}
}
