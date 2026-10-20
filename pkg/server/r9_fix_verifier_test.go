package server

// r9 round witnesses — the provisioning lookups' team-tier strictness, and
// the one end-to-end approve of a bot-set-changing update.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r9BlipBotStore fails its reads while tripped — the transient team-store
// blip, injected at the store the REAL production lookups read, so the chain
// under test is production's own (forgeBotForge → forgeBotEntry →
// storedBotEntry → teamBotRow → this store).
type r9BlipBotStore struct {
	botsource.Store
	trip atomic.Bool
}

func (f *r9BlipBotStore) GetBySlug(ctx context.Context, tenantID, slug string) (botsource.BotSource, error) {
	if f.trip.Load() {
		return botsource.BotSource{}, errors.New("bot catalog store unavailable (blip)")
	}
	return f.Store.GetBySlug(ctx, tenantID, slug)
}

func (f *r9BlipBotStore) ListByTenant(ctx context.Context, tenantID string) ([]botsource.BotSource, error) {
	if f.trip.Load() {
		return nil, errors.New("bot catalog store unavailable (blip)")
	}
	return f.Store.ListByTenant(ctx, tenantID)
}

// A team-authored bot approved while the team's bot-source store blips is a
// TRANSIENT failure, not a fact about the recorded bot set: the launch
// resolver reads that tier strictly (a store error surfaces, it never falls
// through to another tier's answer), and the provisioning lookups are the
// metadata counterpart of the same resolution — so a blip must stay a bare
// 502 with the record pending. Reading the blip as the tier's absence hands
// the approve arm the catalog-miss sentinel, which CONSUMES a record a
// healed retry lands. The control arm is also the suite's only approve of a
// bot-set-CHANGING update carried to completion: it goes through the rebuild
// path's hook PUT against the real GitLab client.
func TestR9_TeamStoreBlip_KeepsTheRecordAHealedRetryLands(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Production lookups; the baked catalog holds review-pr only, so the
	// team-authored bot resolves from the team tier or nowhere.
	r8WireRealBotCatalog(t, s)

	mem := botsource.NewMemoryStore()
	blip := &r9BlipBotStore{Store: mem}
	s.botSources = blip
	tctx := store.WithTenant(context.Background(), "t1")
	if _, err := mem.Create(tctx, botsource.BotSource{
		TenantID: "t1", Slug: "team-bot",
		Files: map[string]string{
			botsource.MainBotFile: r8RealCatalogBot,
			bundle.ManifestFile:   strings.Replace(r8RealCatalogManifest, "name: review-pr", "name: team-bot", 1),
		},
	}); err != nil {
		t.Fatalf("seed team row: %v", err)
	}

	res := seedIntegration(t, s, connID, "group/api", ``)

	// Control: a bot-set-CHANGING update lands healthy through the real
	// lookups — the strict team read serves the team's own bot.
	id := parkUpdate(t, s, res.IntegrationID, `{"schedule_crons":{"review-pr":"0 3 * * *"},"bot_ids":["review-pr","team-bot"]}`)
	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusOK {
		t.Fatalf("control: a healthy approve of a team-authored bot must land: code=%d body=%s", aw.Code, aw.Body.String())
	}
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.BotIDs) != 2 {
		t.Fatalf("control: both bots must land: %v (%v)", ri.BotIDs, err)
	}

	// The blip: the same class of gesture, approved while the team store
	// cannot answer. A transient failure stays bare — 502, record pending.
	blip.trip.Store(true)
	id2 := parkUpdate(t, s, res.IntegrationID, `{"schedule_crons":{"review-pr":"0 4 * * *"}}`)
	aw2 := r6Approve(t, s, id2)
	if aw2.Code != http.StatusBadGateway {
		t.Fatalf("a team-store blip is transient, not a deterministic refusal: code=%d body=%s", aw2.Code, aw2.Body.String())
	}
	if strings.Contains(aw2.Body.String(), "cannot land as recorded") {
		t.Fatalf("a transient blip must not ride the consuming refusal: %s", aw2.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the record must stay pending for the healed retry: %d record(s)", len(got))
	}

	// Heal; the SAME record replays and lands — the retry the stay-pending
	// side of the class boundary exists to preserve.
	blip.trip.Store(false)
	aw3 := r6Approve(t, s, id2)
	if aw3.Code != http.StatusOK {
		t.Fatalf("the healed retry must land the kept record: code=%d body=%s", aw3.Code, aw3.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the healed retry must consume the record it landed: %d record(s) left", len(got))
	}
}

// The strict read has a strict shape: forgeBotForge and forgeBotInvocations
// answer a genuine absence with the catalog-miss sentinel and a tier failure
// with a bare error — both, at the production site (the r8 witnesses pin the
// sentinel through the same lookups; this pins the bare-error arm beside it).
func TestR9_ForgeBotLookups_TierFailureStaysBareNotSentinel(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()

	blip := &r9BlipBotStore{Store: botsource.NewMemoryStore()}
	blip.trip.Store(true)
	s.botSources = blip

	_, forgeErr := s.forgeBotForge(context.Background(), "t1", "team-bot")
	if forgeErr == nil || errors.Is(forgeErr, forge.ErrBotNotProvisionable) {
		t.Fatalf("a team-tier failure must surface bare, never as the catalog-miss sentinel: %v", forgeErr)
	}
	_, invErr := s.forgeBotInvocations(context.Background(), "t1", "team-bot")
	if invErr == nil || errors.Is(invErr, forge.ErrBotNotProvisionable) {
		t.Fatalf("a team-tier failure must surface bare, never as the catalog-miss sentinel: %v", invErr)
	}
}
