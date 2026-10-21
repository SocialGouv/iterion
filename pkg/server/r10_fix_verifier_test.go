package server

// r10 round witnesses — the provisioning lookups' platform-tier strictness
// (the floor's cached miss may not count as absence), and the mock GitLab's
// hook PUT holding its own convention.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r10PlatBlipStore fails the PLATFORM tenant's reads while tripped — the
// transient platform-store failure, injected at the store the real chain
// reads (forgeBotForge → forgeBotEntry → the floor's cache fill and its
// strict platform probe, and the launch resolver's platform arm). The team
// tenant keeps answering, so team-authored bots stay on the team path.
type r10PlatBlipStore struct {
	botsource.Store
	tripList atomic.Bool
	tripGet  atomic.Bool
}

func (f *r10PlatBlipStore) GetBySlug(ctx context.Context, tenantID, slug string) (botsource.BotSource, error) {
	if f.tripGet.Load() && tenantID == botsource.PlatformTenantID {
		return botsource.BotSource{}, errors.New("bot catalog store unavailable (platform blip)")
	}
	return f.Store.GetBySlug(ctx, tenantID, slug)
}

func (f *r10PlatBlipStore) ListByTenant(ctx context.Context, tenantID string) ([]botsource.BotSource, error) {
	if f.tripList.Load() && tenantID == botsource.PlatformTenantID {
		return nil, errors.New("bot catalog store unavailable (platform blip)")
	}
	return f.Store.ListByTenant(ctx, tenantID)
}

func r10SeedPlatformBot(t *testing.T, mem *botsource.MemoryStore, slug string) {
	t.Helper()
	ptctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	if _, err := mem.Create(ptctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID, Slug: slug,
		Files: map[string]string{
			botsource.MainBotFile: r8RealCatalogBot,
			bundle.ManifestFile:   strings.Replace(r8RealCatalogManifest, "name: review-pr", "name: "+slug, 1),
		},
	}); err != nil {
		t.Fatalf("seed platform row %s: %v", slug, err)
	}
}

// A platform-authored bot approved while the platform tier cannot answer is
// a TRANSIENT failure, not a fact about the recorded bot set: the launch
// resolver reads that tier strictly, and the provisioning lookups are the
// metadata counterpart of the same resolution — so a blip must stay a bare
// 502 with the record pending. The floor (effectiveFindByName) cannot make
// that call for it: its platform half is a cache that serves its last-known
// set on a failed refresh, so reading its miss as absence hands the approve
// arm the catalog-miss sentinel, which consumes a record a healed retry
// lands.
func TestR10_PlatformStoreBlip_KeepsTheRecordAHealedRetryLands(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Production lookups; the baked catalog holds review-pr only, so the
	// platform-authored bot resolves from the platform tier or nowhere.
	r8WireRealBotCatalog(t, s)
	mem := botsource.NewMemoryStore()
	blip := &r10PlatBlipStore{Store: mem}
	s.botSources = blip

	// Seed first: its own reads leave the platform cache holding the
	// pre-row (empty) set — the cold/cross-replica shape the blip models.
	res := seedIntegration(t, s, connID, "group/api", ``)
	r10SeedPlatformBot(t, mem, "plat-bot")

	// THE BLIP: the platform tier cannot answer while the cache holds the
	// pre-row set (cold replica, cross-replica write, refetch blip).
	blip.tripList.Store(true)
	blip.tripGet.Store(true)
	s.platformBots.Invalidate()

	// The strict read has a strict shape: a platform-tier failure surfaces
	// bare, never as the catalog-miss sentinel — at the production site.
	if _, err := s.forgeBotForge(context.Background(), "t1", "plat-bot"); err == nil || errors.Is(err, forge.ErrBotNotProvisionable) {
		t.Fatalf("a platform-tier failure must surface bare, never as the catalog-miss sentinel: %v", err)
	}
	if _, err := s.forgeBotInvocations(context.Background(), "t1", "plat-bot"); err == nil || errors.Is(err, forge.ErrBotNotProvisionable) {
		t.Fatalf("a platform-tier failure must surface bare, never as the catalog-miss sentinel: %v", err)
	}

	// END TO END: the blipped approve stays pending behind the 502.
	id := parkUpdate(t, s, res.IntegrationID, `{"schedule_crons":{"review-pr":"0 3 * * *"},"bot_ids":["review-pr","plat-bot"]}`)
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the record must be parked before the approve: %d", len(got))
	}
	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusBadGateway {
		t.Fatalf("a platform-store blip is transient, not a deterministic refusal: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if strings.Contains(aw.Body.String(), "cannot land as recorded") {
		t.Fatalf("a transient blip must not ride the consuming refusal: %s", aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the record must stay pending for the healed retry: %d record(s)", len(got))
	}

	// Heal; the SAME record replays and lands — the retry the stay-pending
	// side of the class boundary exists to preserve.
	blip.tripList.Store(false)
	blip.tripGet.Store(false)
	s.platformBots.Invalidate()
	aw2 := r6Approve(t, s, id)
	if aw2.Code != http.StatusOK {
		t.Fatalf("the healed retry must land the kept record: code=%d body=%s", aw2.Code, aw2.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the healed retry must consume the record it landed: %d record(s) left", len(got))
	}
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.BotIDs) != 2 {
		t.Fatalf("heal: both bots must land: %v (%v)", ri.BotIDs, err)
	}
}

// The other arm of the same hole: the store answers live, but the floor's
// cached set predates the row (the stale window a cross-replica write or a
// failed refetch opens). The launch resolver finds the bot live; a
// provisioning read that trusted the floor's stale miss would consume the
// record as a deterministic dead end while the bot it names is launchable.
func TestR10_PlatformCacheStale_LiveStoreStillLands(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	r8WireRealBotCatalog(t, s)
	mem := botsource.NewMemoryStore()
	stale := &r10PlatBlipStore{Store: mem}
	s.botSources = stale

	// Seed first: its own reads leave the platform cache holding the
	// pre-row (empty) set — the stale window.
	res := seedIntegration(t, s, connID, "group/api", ``)
	r10SeedPlatformBot(t, mem, "plat-bot")

	// The store answers live; only the floor's cache-fill read fails.
	stale.tripList.Store(true)
	s.platformBots.Invalidate()

	// The launch resolver DISAGREES with the floor: it live-reads the tier.
	lb, lerr := s.resolveBotTiered(context.Background(), "t1", "plat-bot", "")
	if lerr != nil || lb == nil {
		t.Fatalf("the launch resolver live-reads the tier (found or error): lb=%v err=%v", lb, lerr)
	}
	lb.Cleanup()

	// The approve of a gesture naming the platform bot must LAND.
	id := parkUpdate(t, s, res.IntegrationID, `{"schedule_crons":{"review-pr":"0 5 * * *"},"bot_ids":["review-pr","plat-bot"]}`)
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the record must be parked before the approve: %d", len(got))
	}
	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusOK {
		t.Fatalf("a stale floor must not consume a record the live store can land: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the landed approve must consume its own record: %d left", len(got))
	}
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.BotIDs) != 2 {
		t.Fatalf("both bots must land through the stale window: %v (%v)", ri.BotIDs, err)
	}
}

// ---- the mock GitLab's hook PUT convention ----

func r10MockPut(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build PUT: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// A body the mock cannot decode is a 400 and stores nothing (a nil map
// cannot take the echoed id), and a malformed path id is a 404 that stores
// nothing under id 0.
func TestR10_MockHookPUT_RefusesMalformedRequests(t *testing.T) {
	gl := newMockGitLab()
	ts := gl.server()
	defer ts.Close()
	base := ts.URL + "/api/v4/projects/group%2Fapi/hooks/"

	resp := r10MockPut(t, base+"7", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an empty body must be a 400, not a handler panic: code=%d", resp.StatusCode)
	}
	if len(gl.hooks) != 0 {
		t.Fatalf("a refused body must store nothing: %v", gl.hooks)
	}

	resp = r10MockPut(t, base+"not-a-number", `{"url":"https://x"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a malformed path id must be a 404: code=%d", resp.StatusCode)
	}
	if len(gl.hooks) != 0 {
		t.Fatalf("a refused id must store nothing under id 0: %v", gl.hooks)
	}

	// The success path still echoes the hook with its id.
	resp = r10MockPut(t, base+"7", `{"url":"https://x"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a well-formed PUT must still succeed: code=%d", resp.StatusCode)
	}
	if got := gl.hooks[7]; got == nil || got["url"] != "https://x" {
		t.Fatalf("the echoed hook must be stored under its id: %v", got)
	}
}

// The PUT branch mutates the same hooks map the POST and DELETE branches
// guard with the mock's mutex; run under -race it must stay race-free.
func TestR10_MockHookPUT_ConcurrentWithSiblings_RaceFree(t *testing.T) {
	gl := newMockGitLab()
	ts := gl.server()
	defer ts.Close()
	base := ts.URL + "/api/v4/projects/group%2Fapi/hooks"

	var wg sync.WaitGroup
	errs := make(chan error, 180)
	for range 60 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			resp, err := http.Post(base, "application/json", strings.NewReader(`{"url":"https://x"}`))
			if err != nil {
				errs <- err
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("POST: code=%d", resp.StatusCode)
			}
		}()
		go func() {
			defer wg.Done()
			resp, err := http.Post(base, "application/json", strings.NewReader(`{"url":"https://y"}`))
			if err != nil {
				errs <- err
				return
			}
			_ = resp.Body.Close()
			req, _ := http.NewRequest(http.MethodPut, base+"/1", strings.NewReader(`{"url":"https://z"}`))
			resp2, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			_ = resp2.Body.Close()
		}()
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodDelete, base+"/1", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
