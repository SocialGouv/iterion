package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// poolFoldServer is the routing fold's test server with the pool stores
// wired: a broker on memory stores and the attachment memory twin. The
// org/team identity fixtures come from newOrgTestServer.
func poolFoldServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(context.Background(), credpool.Pool{ID: "pool-a", OrgID: "org1", Enabled: true}); err != nil {
		t.Fatalf("seed pool-a: %v", err)
	}
	if err := pools.Upsert(context.Background(), credpool.Pool{ID: "pool-b", OrgID: "org1", Enabled: true}); err != nil {
		t.Fatalf("seed pool-b: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools:   pools,
		Pledges: credpool.NewMemoryPledgeStore(),
		Leases:  credpool.NewMemoryLeaseStore(),
		Ledger:  credpool.NewMemoryLedger(),
		OAuth:   secrets.NewMemoryOAuthStore(),
		Sealer:  sealer,
		Logger:  iterlog.New(iterlog.LevelError, nil),
	})
	if broker == nil {
		t.Fatal("broker nil with every dependency wired")
	}
	s.credPool = broker
	s.credPoolAttachments = credpool.NewMemoryAttachmentStore()
	return s
}

func seedPoolFoldTeam(t *testing.T, s *Server, teamID string) {
	t.Helper()
	seedOrg(t, s, "org1", "org1")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: teamID, Name: teamID, Slug: teamID, OrgID: "org1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
}

func putAttachment(t *testing.T, s *Server, scopeID, poolID string, mutate ...func(*credpool.Attachment)) {
	t.Helper()
	a := credpool.Attachment{ScopeID: scopeID, PoolID: poolID, Enabled: true}
	for _, m := range mutate {
		m(&a)
	}
	if err := s.credPoolAttachments.Put(context.Background(), a); err != nil {
		t.Fatalf("put attachment %s: %v", scopeID, err)
	}
}

// Nearest wins, in both write orders: a team attachment answers over its
// org's whatever order the records were written in (the fold reads the
// stores, not a write sequence), and a binding layer outranks both.
func TestResolveRunPoolAttachment_NearestWinsInBothOrders(t *testing.T) {
	s := poolFoldServer(t)
	seedPoolFoldTeam(t, s, "team1")
	putAttachment(t, s, credpool.OrgScopeID("org1"), "pool-b")
	putAttachment(t, s, credpool.TeamScopeID("team1"), "pool-a")
	got, err := s.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err != nil || got == nil {
		t.Fatalf("resolve = (%v, %v)", got, err)
	}
	if got.PoolID != "pool-a" {
		t.Errorf("pool = %q, want the team's pool-a (org written first, team second)", got.PoolID)
	}

	// The other write order on a fresh fixture: team written first, org
	// second — the same answer, the fold must not depend on write sequence.
	s2 := poolFoldServer(t)
	seedPoolFoldTeam(t, s2, "team1")
	putAttachment(t, s2, credpool.TeamScopeID("team1"), "pool-a")
	putAttachment(t, s2, credpool.OrgScopeID("org1"), "pool-b")
	got2, err := s2.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err != nil || got2 == nil || got2.PoolID != "pool-a" {
		t.Fatalf("other write order = (%v, %v), want pool-a", got2, err)
	}

	// The org record gone, the team still answers; the team record gone,
	// the org answers.
	if err := s.credPoolAttachments.Delete(context.Background(), credpool.TeamScopeID("team1")); err != nil {
		t.Fatalf("detach team: %v", err)
	}
	got3, err := s.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err != nil || got3 == nil || got3.PoolID != "pool-b" {
		t.Fatalf("after team detach = (%v, %v), want the org's pool-b (detach = inherit)", got3, err)
	}

	// A binding layer outranks every stored level.
	got4, err := s.resolveRunPoolAttachment(context.Background(), "team1", "", credpool.BindingLayer{
		Source: "webhook", Enabled: true, PoolID: "pool-a",
	})
	if err != nil || got4 == nil || got4.PoolID != "pool-a" {
		t.Fatalf("binding layer = (%v, %v), want pool-a over the org record", got4, err)
	}
}

// No attachment anywhere = nil: today's pool behavior byte for byte (law 2
// — zero pool without an explicit opt-in).
func TestResolveRunPoolAttachment_NoAttachmentAnswersNil(t *testing.T) {
	s := poolFoldServer(t)
	seedPoolFoldTeam(t, s, "team1")
	got, err := s.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err != nil || got != nil {
		t.Fatalf("resolve = (%v, %v), want (nil, nil)", got, err)
	}
	// The platform floor answers when the team/org stay silent.
	putAttachment(t, s, credpool.ScopePlatform, "pool-a")
	got, err = s.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err != nil || got == nil || got.PoolID != "pool-a" {
		t.Fatalf("platform floor = (%v, %v), want pool-a", got, err)
	}
	// A team-less launch (the platform admin view) walks platform only —
	// and with no platform record answers nil.
	got, err = s.resolveRunPoolAttachment(context.Background(), "", "")
	if err != nil || got == nil || got.PoolID != "pool-a" {
		t.Fatalf("admin view = (%v, %v), want the platform attachment", got, err)
	}
}

// Enabled=false is a resolved-disabled answer: the fold stops there and
// the parent's pool does NOT serve — nearest-wins means nearest also
// blocks.
func TestResolveRunPoolAttachment_DisabledBlocksInheritance(t *testing.T) {
	s := poolFoldServer(t)
	seedPoolFoldTeam(t, s, "team1")
	putAttachment(t, s, credpool.OrgScopeID("org1"), "pool-b")
	putAttachment(t, s, credpool.TeamScopeID("team1"), "pool-a", func(a *credpool.Attachment) { a.Enabled = false })
	got, err := s.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil {
		t.Fatal("snapshot = nil, want the disabled answer (non-nil, empty pool)")
	}
	if got.PoolID != "" || len(got.Strategy.Triggers) != 0 {
		t.Errorf("disabled answer = %+v, want an empty pool and an empty trigger list — the run draws from no pool at all", got)
	}
}

// A vanished or disabled pool refuses the launch, naming its level — the
// routing fold's garbage rule applied to the pool the attachment names.
func TestResolveRunPoolAttachment_VanishedPoolRefusesNamingTheLevel(t *testing.T) {
	s := poolFoldServer(t)
	seedPoolFoldTeam(t, s, "team1")
	putAttachment(t, s, credpool.TeamScopeID("team1"), "pool-gone")
	_, err := s.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err == nil || !strings.Contains(err.Error(), "team level") || !strings.Contains(err.Error(), "pool-gone") {
		t.Fatalf("resolve err = %v, want a refusal naming the team level and the pool", err)
	}
	// An unknown trigger refuses too, naming the level.
	s2 := poolFoldServer(t)
	seedPoolFoldTeam(t, s2, "team1")
	putAttachment(t, s2, credpool.TeamScopeID("team1"), "pool-a", func(a *credpool.Attachment) {
		a.Strategy.Triggers = []string{"margins"}
	})
	_, err = s2.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err == nil || !strings.Contains(err.Error(), "team level") || !strings.Contains(err.Error(), "margins") {
		t.Fatalf("garbage trigger err = %v, want a refusal naming the level and the trigger", err)
	}
}

// The platform attachment's triggers are the ceiling: a team asking for
// more than the platform allows resolves to the narrowed list — never a
// silent extension. The winner's EMPTY list freezes as the pool's default
// [usage_window].
func TestResolveRunPoolAttachment_PlatformCeilingAndDefaults(t *testing.T) {
	s := poolFoldServer(t)
	seedPoolFoldTeam(t, s, "team1")
	putAttachment(t, s, credpool.ScopePlatform, "pool-a", func(a *credpool.Attachment) {
		a.Strategy.Triggers = []string{llmroute.TriggerUsageWindow}
	})
	putAttachment(t, s, credpool.TeamScopeID("team1"), "pool-b", func(a *credpool.Attachment) {
		a.Strategy.Triggers = []string{llmroute.TriggerUsageWindow, llmroute.TriggerUnavailable}
	})
	got, err := s.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err != nil || got == nil {
		t.Fatalf("resolve = (%v, %v)", got, err)
	}
	if got.PoolID != "pool-b" || len(got.Strategy.Triggers) != 1 || got.Strategy.Triggers[0] != llmroute.TriggerUsageWindow {
		t.Errorf("frozen = pool %q triggers %v, want pool-b narrowed to [usage_window] by the platform ceiling", got.PoolID, got.Strategy.Triggers)
	}

	// No triggers anywhere freezes the pool's default.
	s2 := poolFoldServer(t)
	seedPoolFoldTeam(t, s2, "team1")
	putAttachment(t, s2, credpool.TeamScopeID("team1"), "pool-a")
	got2, err := s2.resolveRunPoolAttachment(context.Background(), "team1", "")
	if err != nil || got2 == nil {
		t.Fatalf("resolve = (%v, %v)", got2, err)
	}
	if len(got2.Strategy.Triggers) != 1 || got2.Strategy.Triggers[0] != llmroute.TriggerUsageWindow {
		t.Errorf("default triggers = %v, want [usage_window]", got2.Strategy.Triggers)
	}
	// The margin freezes as carried (nil = the DefaultMarginPct reading at
	// the consumer; a set margin freezes set).
	if got2.Strategy.MarginPct != nil {
		t.Errorf("a nil margin must freeze nil (the default is the consumer's reading): %v", *got2.Strategy.MarginPct)
	}
	var _ = store.RunPoolAttachment{} // import anchor
}
