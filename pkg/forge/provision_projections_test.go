package forge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// ---- the spine fixtures: three label bots, a fence probe, a fake projector ----

// spineBotLookup serves the three issue-label bots of the fixtures. nestor and
// saboteur name their labels in a kind=forge invocation; feature-dev claims
// issue_labeled through its forge: block alone, with a manifest launch var.
// Any other id is the package's shared fixture catalogue's (testBotLookup).
func spineBotLookup(ctx context.Context, team, botID string) (*bundle.ForgeRequirements, error) {
	switch botID {
	case "nestor", "saboteur":
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventIssueLabeled},
			TokenScopes: map[string]string{"issues": "write"},
			Secret:      "forge_token",
		}, nil
	case "feature-dev":
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventIssueLabeled},
			TokenScopes: map[string]string{"issues": "write", "repository": "write"},
			Secret:      "forge_token",
			Webhook:     &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"open_mr": "true"}},
		}, nil
	}
	return testBotLookup(ctx, team, botID)
}

func spineInvocations(_ context.Context, _, botID string) ([]bundle.Invocation, error) {
	switch botID {
	case "nestor":
		return []bundle.Invocation{
			{Kind: bundle.InvocationKindForge, Forge: &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{"nestor"}}},
			{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"}},
		}, nil
	case "saboteur":
		return []bundle.Invocation{
			{Kind: bundle.InvocationKindForge, Mode: "board", Forge: &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{"saboteur"}}},
		}, nil
	}
	return nil, nil
}

// fenceProbe records every provision fence the orchestrator opens on the
// integration store and whether one is running — what "inside the fence"
// means for the projector below. Goroutines feed it (the lease renewer shares
// the store), so every field is read and written under mu.
type fenceProbe struct {
	RepoIntegrationStore
	mu      sync.Mutex
	fences  []fenceCall
	running bool
}

type fenceCall struct {
	version int64
	retired bool
	err     error
}

func (p *fenceProbe) WithProvisionVersion(ctx context.Context, id string, version int64, retired bool, write func(context.Context) error) error {
	err := p.RepoIntegrationStore.WithProvisionVersion(ctx, id, version, retired, func(ctx context.Context) error {
		p.setRunning(true)
		defer p.setRunning(false)
		return write(ctx)
	})
	p.mu.Lock()
	p.fences = append(p.fences, fenceCall{version: version, retired: retired, err: err})
	p.mu.Unlock()
	return err
}

func (p *fenceProbe) setRunning(v bool) {
	p.mu.Lock()
	p.running = v
	p.mu.Unlock()
}

func (p *fenceProbe) isRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// take returns the fences opened since the last take.
func (p *fenceProbe) take() []fenceCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.fences
	p.fences = nil
	return out
}

// fakeProjector stands in for the trigger-spine projector, which lives in a
// package this one cannot import. It keeps one "row" per bot — the labels the
// row claims — computed the way the real planner reads its input: the
// operator's BotTriggers labels when set, else the manifest's forge.labels.
// It records each call, and whether the call ran inside a provision fence.
type fakeProjector struct {
	probe *fenceProbe

	mu          sync.Mutex
	rows        map[string]map[string][]string // integration id → bot → the labels its row claims
	syncs       []SubscriptionInput
	syncInFence []bool
	drops       []string
	dropInFence []bool
	// refuse, when set, replaces the projection: its report and error are
	// returned as they are, and no row changes.
	refuse func(SubscriptionInput) (SubscriptionReport, error)
}

func newFakeProjector(probe *fenceProbe) *fakeProjector {
	return &fakeProjector{probe: probe, rows: map[string]map[string][]string{}}
}

func (f *fakeProjector) SyncIntegration(_ context.Context, in SubscriptionInput) (SubscriptionReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, in)
	f.syncInFence = append(f.syncInFence, f.probe.isRunning())
	if f.refuse != nil {
		return f.refuse(in)
	}
	report := SubscriptionReport{
		Enabled:  map[string]map[string][]string{webhooks.LaneIssues: {}},
		Disabled: map[string]string{},
		Digest:   "digest-" + strings.Join(in.Bots, "+"),
	}
	rows := map[string][]string{}
	adopting := in.Integration.SubscriptionsAdoptedAt == nil
	residualTaken := false
	for _, bot := range in.Bots {
		trigger, held := in.Integration.BotTriggers[bot]
		labels := trigger.Labels
		if labels == nil {
			for _, inv := range in.InvByBot[bot] {
				if l, _ := inv.ForgeTriggerLabels(); l != nil {
					labels = l
				}
			}
		}
		enabled := labels != nil || trigger.Residual
		if trigger.Enabled != nil {
			enabled = *trigger.Enabled
		}
		if !held && labels == nil && adopting {
			// The first label-less bot is the residual winner, the others are
			// frozen off: the shape of the real adoption.
			if !residualTaken {
				residualTaken, enabled = true, true
				report.AdoptionPins = pinBot(report.AdoptionPins, bot, BotTrigger{Residual: true})
			} else {
				off := false
				report.AdoptionPins = pinBot(report.AdoptionPins, bot, BotTrigger{Enabled: &off})
			}
		}
		if !enabled {
			report.Disabled[bot] = "declares issue_labeled with no label"
			continue
		}
		claimed := append([]string(nil), labels...)
		if len(claimed) == 0 {
			claimed = []string{"*"}
		}
		rows[bot] = claimed
		report.Enabled[webhooks.LaneIssues][bot] = claimed
	}
	f.rows[in.Integration.ID] = rows
	return report, nil
}

func pinBot(pins map[string]BotTrigger, bot string, pin BotTrigger) map[string]BotTrigger {
	if pins == nil {
		pins = map[string]BotTrigger{}
	}
	pins[bot] = pin
	return pins
}

func (f *fakeProjector) DropIntegration(_ context.Context, _, integrationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops = append(f.drops, integrationID)
	f.dropInFence = append(f.dropInFence, f.probe.isRunning())
	delete(f.rows, integrationID)
	return nil
}

func (f *fakeProjector) rowLabels(integrationID, bot string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rows[integrationID][bot]...)
}

func (f *fakeProjector) hasRows(integrationID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[integrationID]
	return ok
}

// takeSyncs returns the SyncIntegration calls made since the last take, with
// whether each ran inside a fence.
func (f *fakeProjector) takeSyncs() ([]SubscriptionInput, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ins, in := f.syncs, f.syncInFence
	f.syncs, f.syncInFence = nil, nil
	return ins, in
}

func (f *fakeProjector) takeDrops() ([]string, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids, in := f.drops, f.dropInFence
	f.drops, f.dropInFence = nil, nil
	return ids, in
}

func (f *fakeProjector) setRefuse(fn func(SubscriptionInput) (SubscriptionReport, error)) {
	f.mu.Lock()
	f.refuse = fn
	f.mu.Unlock()
}

// spineOrch is the prod shape: an orchestrator that wires BOTH projections —
// the schedule store and the subscription projector — over a fence probe, with
// a clock the test moves.
type spineOrch struct {
	*Orchestrator
	probe     *fenceProbe
	projector *fakeProjector
	clock     *atomic.Int64
	warnings  *warnLog
}

type warnLog struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnLog) add(format string, args ...any) {
	w.mu.Lock()
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
	w.mu.Unlock()
}

func (w *warnLog) containing(sub string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, l := range w.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func newSpineOrch(t *testing.T) *spineOrch {
	t.Helper()
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	return wireSpine(o)
}

// wireSpine wires the two projections and the fixtures' lookups on o, over the
// stores o already holds.
func wireSpine(o *Orchestrator) *spineOrch {
	s := &spineOrch{Orchestrator: o, clock: &atomic.Int64{}, warnings: &warnLog{}}
	s.clock.Store(1700000000)
	o.Now = func() time.Time { return time.Unix(s.clock.Load(), 0).UTC() }
	o.Bots = spineBotLookup
	o.Invocations = spineInvocations
	if o.Schedules == nil {
		o.Schedules = cloudsched.NewMemoryStore()
	}
	s.probe = &fenceProbe{RepoIntegrationStore: o.Integrations}
	o.Integrations = s.probe
	s.projector = newFakeProjector(s.probe)
	o.Subscriptions = s.projector
	o.LogWarn = s.warnings.add
	return s
}

// hour moves the clock one hour on and returns the new instant.
func (s *spineOrch) hour() time.Time {
	return time.Unix(s.clock.Add(3600), 0).UTC()
}

func (s *spineOrch) now() time.Time { return time.Unix(s.clock.Load(), 0).UTC() }

func (s *spineOrch) provision(t *testing.T, req ProvisionRequest) ProvisionResult {
	t.Helper()
	req.TenantID, req.ConnectionID, req.RepoFullName, req.ActorID = "t1", "conn-1", "group/api", "u1"
	res, err := s.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("provision %v: %v", req.BotIDs, err)
	}
	return res
}

func (s *spineOrch) stored(t *testing.T, res ProvisionResult) (RepoIntegration, webhooks.Config) {
	t.Helper()
	ri, err := s.Integrations.GetProvision(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatalf("read integration back: %v", err)
	}
	cfg, err := s.Webhooks.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatalf("read webhook config back: %v", err)
	}
	return ri, cfg
}

func (s *spineOrch) scheduleIDs(t *testing.T, integrationID string) []string {
	t.Helper()
	rows, err := s.Schedules.ListByIntegration(context.Background(), "t1", integrationID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// flipToSpine switches the issue lane to spine through the public switch.
func (s *spineOrch) flipToSpine(t *testing.T, res ProvisionResult) RoutingResult {
	t.Helper()
	out, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if err != nil {
		t.Fatalf("flip the issue lane to spine: %v", err)
	}
	return out
}

// ---- one fence for both projections ----

// oneFenceStory drives the three call sites that project — the rebuild, the
// idempotent short-circuit and Deprovision — on an orchestrator wiring both
// projections, and holds each to: both projections landed, the subscription
// one inside the fence; the stamps are written; exactly one fence was opened
// and the integration's version advanced by exactly one over it.
//
// A fence is single-use. Two fences on one version, or the stamps written by a
// whole-document Update inside the fence, answer ErrIntegrationConflict on any
// server that wires schedules — which every cloud deployment does.
func oneFenceStory(t *testing.T, s *spineOrch) {
	ctx := context.Background()

	// --- the rebuild path: the first enable ---
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "feature-dev"}})
	fences := s.probe.take()
	if len(fences) != 1 || fences[0].retired || fences[0].err != nil {
		t.Fatalf("rebuild: the provision opened fences %+v, want exactly one, successful", fences)
	}
	syncs, inFence := s.projector.takeSyncs()
	if len(syncs) != 1 || !inFence[0] {
		t.Fatalf("rebuild: %d subscription projection(s), in a fence: %v; want one, inside the fence", len(syncs), inFence)
	}
	if got := syncs[0]; got.Integration.ID != res.IntegrationID || got.Config.ID != res.WebhookID || !slices.Equal(got.Bots, []string{"feature-dev", "nestor"}) {
		t.Fatalf("rebuild: the projection read integration %q, config %q, bots %v", got.Integration.ID, got.Config.ID, got.Bots)
	}
	if got := syncs[0]; got.FrByBot["feature-dev"] == nil || len(got.InvByBot["nestor"]) != 2 {
		t.Fatalf("rebuild: the projection was not handed the manifests Provision resolved: fr %v, inv %v", got.FrByBot, got.InvByBot)
	}
	ri, _ := s.stored(t, res)
	if ri.Version != fences[0].version+1 {
		t.Fatalf("rebuild: version %d after a fence opened at %d: it must advance exactly once", ri.Version, fences[0].version)
	}
	schedules := s.scheduleIDs(t, res.IntegrationID)
	if len(schedules) != 1 {
		t.Fatalf("rebuild: %d scheduled row(s), want nestor's one", len(schedules))
	}
	if got := s.projector.rowLabels(res.IntegrationID, "nestor"); !slices.Equal(got, []string{"nestor"}) {
		t.Fatalf("rebuild: nestor's projected row claims %v", got)
	}
	first := s.now()
	if !sameInstantPtr(ri.SubscriptionsSyncedAt, first) || !sameInstantPtr(ri.SubscriptionsAdoptedAt, first) || ri.SubscriptionsSyncError != "" {
		t.Fatalf("rebuild: stamps = synced %v, adopted %v, error %q; want both instants at %v and no error", ri.SubscriptionsSyncedAt, ri.SubscriptionsAdoptedAt, ri.SubscriptionsSyncError, first)
	}
	if !ri.BotTriggers["feature-dev"].Residual {
		t.Fatalf("rebuild: the adoption pin was not stamped: bot triggers = %+v", ri.BotTriggers)
	}

	// --- the idempotent short-circuit: the same request again ---
	second := s.hour()
	before := ri.Version
	again := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "feature-dev"}})
	if again.Created {
		t.Fatal("short-circuit: the identical request took the rebuild path, so this row proves nothing about the short-circuit")
	}
	fences = s.probe.take()
	if len(fences) != 1 || fences[0].retired || fences[0].err != nil {
		t.Fatalf("short-circuit: fences %+v, want exactly one, successful", fences)
	}
	syncs, inFence = s.projector.takeSyncs()
	if len(syncs) != 1 || !inFence[0] {
		t.Fatalf("short-circuit: %d subscription projection(s), in a fence: %v; want one, inside the fence", len(syncs), inFence)
	}
	if !syncs[0].Integration.BotTriggers["feature-dev"].Residual || syncs[0].Integration.SubscriptionsAdoptedAt == nil {
		t.Fatalf("short-circuit: the projection read an integration without its adoption state: %+v", syncs[0].Integration)
	}
	ri, _ = s.stored(t, res)
	if ri.Version != before+1 {
		t.Fatalf("short-circuit: version %d → %d: an unchanged re-provision writes the integration through its one fence only", before, ri.Version)
	}
	if got := s.scheduleIDs(t, res.IntegrationID); !slices.Equal(got, schedules) {
		t.Fatalf("short-circuit: scheduled rows %v, want the carried %v", got, schedules)
	}
	if !sameInstantPtr(ri.SubscriptionsSyncedAt, second) || !sameInstantPtr(ri.SubscriptionsAdoptedAt, first) {
		t.Fatalf("short-circuit: synced %v (want %v), adopted %v (want it kept at %v)", ri.SubscriptionsSyncedAt, second, ri.SubscriptionsAdoptedAt, first)
	}

	// --- Deprovision: the rows go inside the retirement fence ---
	if err := s.Deprovision(ctx, "t1", res.IntegrationID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	fences = s.probe.take()
	if len(fences) != 1 || !fences[0].retired || fences[0].err != nil {
		t.Fatalf("deprovision: fences %+v, want exactly one, on the retired row, successful", fences)
	}
	drops, dropInFence := s.projector.takeDrops()
	if len(drops) != 1 || drops[0] != res.IntegrationID || !dropInFence[0] {
		t.Fatalf("deprovision: drops %v, in a fence: %v; want this integration's rows dropped once, inside the fence", drops, dropInFence)
	}
	if s.projector.hasRows(res.IntegrationID) {
		t.Fatal("deprovision: the integration's subscription rows survive it")
	}
	if got := s.scheduleIDs(t, res.IntegrationID); len(got) != 0 {
		t.Fatalf("deprovision: scheduled rows %v survive it", got)
	}
}

func TestProvision_ProjectsSchedulesAndSubscriptionsInOneFence(t *testing.T) {
	oneFenceStory(t, newSpineOrch(t))
}

// newMongoSpineOrch is newSpineOrch on the stores that run in production: the
// integration store (whose fence is a transaction and whose stamps are
// pipeline updates), the webhook config store and the schedule store, all on
// one database so their writes join the fence. It skips the test when no
// Mongo is offered (ITERION_TEST_MONGO_URI; ./pkg/forge/... is in the
// mongo-conformance job's package list).
func newMongoSpineOrch(t *testing.T) *spineOrch {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo twin of this story")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo.Connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_forge_spine_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	integrations := NewMongoRepoIntegrationStore(db)
	if err := integrations.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := webhooks.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("webhooks.EnsureSchema: %v", err)
	}
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	o.Integrations = integrations
	o.Webhooks = webhooks.NewMongoStores(db).Configs
	o.Schedules = cloudsched.NewMongoStore(db)
	return wireSpine(o)
}

// The same story on the stores that run in production.
func TestProvision_ProjectsSchedulesAndSubscriptionsInOneFence_Mongo(t *testing.T) {
	oneFenceStory(t, newMongoSpineOrch(t))
}

// Either projection alone opens the fence: a server that wires the projector
// and no schedule store still projects on a provision and still drops the
// rows on a deprovision.
func TestProvision_ProjectsSubscriptionsWithoutAScheduleStore(t *testing.T) {
	s := newSpineOrch(t)
	s.Schedules = nil
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	if fences := s.probe.take(); len(fences) != 1 || fences[0].err != nil {
		t.Fatalf("provision: fences %+v, want one", fences)
	}
	if syncs, inFence := s.projector.takeSyncs(); len(syncs) != 1 || !inFence[0] {
		t.Fatalf("provision: %d projection(s), in a fence: %v; want one, inside the fence", len(syncs), inFence)
	}
	if ri, _ := s.stored(t, res); ri.SubscriptionsSyncedAt == nil || ri.SubscriptionsAdoptedAt == nil {
		t.Fatalf("provision: the sync was not stamped: %+v", ri)
	}
	if err := s.Deprovision(context.Background(), "t1", res.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if fences := s.probe.take(); len(fences) != 1 || !fences[0].retired {
		t.Fatalf("deprovision: fences %+v, want one on the retired row", fences)
	}
	if drops, inFence := s.projector.takeDrops(); len(drops) != 1 || !inFence[0] || s.projector.hasRows(res.IntegrationID) {
		t.Fatalf("deprovision: drops %v, in a fence: %v — the rows of a retired integration survive it", drops, inFence)
	}
}

// The version the fence leaves is what syncProvisionProjections returns: its
// caller assigns it, and a caller that kept the version it passed in would
// conflict on its next write.
func TestSyncProvisionProjections_ReturnsTheVersionTheFenceLeaves(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	ri, cfg := s.stored(t, res)
	frByBot, invByBot, err := s.resolveBotManifests(context.Background(), "t1", ri.BotIDs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.syncProvisionProjections(context.Background(), ri, cfg, "", ri.BotIDs, invByBot, frByBot, nil, "u1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.stored(t, res)
	if got != after.Version || got != ri.Version+1 {
		t.Fatalf("returned version %d; the store holds %d after a fence opened at %d", got, after.Version, ri.Version)
	}
	// The returned version is the one the next write needs.
	after.MinAuthorRole = "maintainer"
	after.Version = got
	if err := s.Integrations.Update(context.Background(), after); err != nil {
		t.Fatalf("a write at the returned version: %v", err)
	}

	// With neither projection wired there is no fence, and no advance.
	s.Schedules, s.Subscriptions = nil, nil
	s.probe.take()
	ri, cfg = s.stored(t, res)
	got, err = s.syncProvisionProjections(context.Background(), ri, cfg, "", ri.BotIDs, invByBot, frByBot, nil, "u1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fences := s.probe.take(); len(fences) != 0 || got != ri.Version {
		t.Fatalf("nothing wired: fences %+v, returned version %d (stored %d); want no fence and the version unchanged", fences, got, ri.Version)
	}
}

// ---- the error policy of a refused projection ----

var errRowsOverlap = errors.New("planner: rows overlap")

// On an integration no lane of which routes by the spine, the rows are inert:
// a refused projection is recorded, warned, and does not cost the provision
// its schedules.
func TestProvision_RefusedProjection_OnLegacyIsRecordedAndSchedulesCommit(t *testing.T) {
	s := newSpineOrch(t)
	s.projector.setRefuse(func(SubscriptionInput) (SubscriptionReport, error) {
		return SubscriptionReport{Overlaps: []string{"nestor: a ∩ b"}}, errRowsOverlap
	})
	for _, path := range []string{"rebuild", "short-circuit"} {
		at := s.hour()
		res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
		if created := res.Created; created != (path == "rebuild") {
			t.Fatalf("%s: Created = %v — the row did not take the path it names", path, created)
		}
		ri, _ := s.stored(t, res)
		if !strings.Contains(ri.SubscriptionsSyncError, errRowsOverlap.Error()) {
			t.Fatalf("%s: the refusal is not recorded on the integration: subscriptions_sync_error = %q", path, ri.SubscriptionsSyncError)
		}
		if !sameInstantPtr(ri.SubscriptionsSyncedAt, at) {
			t.Fatalf("%s: synced = %v, want the instant of the refused projection %v", path, ri.SubscriptionsSyncedAt, at)
		}
		if ri.SubscriptionsAdoptedAt != nil {
			t.Fatalf("%s: a refused projection adopted (%v)", path, ri.SubscriptionsAdoptedAt)
		}
		if got := s.scheduleIDs(t, res.IntegrationID); len(got) != 1 {
			t.Fatalf("%s: %d scheduled row(s): the refused subscription projection cost the provision its schedules", path, len(got))
		}
	}
	if n := s.warnings.containing(errRowsOverlap.Error()); n != 2 {
		t.Fatalf("the refusal was warned %d time(s), want once per provision: it must never stay silent", n)
	}
}

// On an integration that routes a lane by the spine the rows decide what a
// delivery starts: a refused projection fails the provision, and the fence
// rolls back whatever it stamped.
func TestProvision_RefusedProjection_OnSpineFailsTheProvision(t *testing.T) {
	for _, path := range []string{"short-circuit", "rebuild"} {
		t.Run(path, func(t *testing.T) {
			s := newSpineOrch(t)
			res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
			s.flipToSpine(t, res)
			before, _ := s.stored(t, res)
			if before.SubscriptionsSyncedAt == nil {
				t.Fatal("precondition: the flip did not stamp its subscription sync")
			}
			s.hour()
			s.projector.setRefuse(func(SubscriptionInput) (SubscriptionReport, error) {
				return SubscriptionReport{}, errRowsOverlap
			})
			bots := []string{"nestor"}
			if path == "rebuild" {
				bots = []string{"nestor", "saboteur"}
			}
			s.probe.take()
			_, err := s.Provision(context.Background(), ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: bots, ActorID: "u1"})
			if !errors.Is(err, ErrSubscriptionProjection) || !errors.Is(err, errRowsOverlap) {
				t.Fatalf("a refused projection on a spine-routed integration: err = %v, want ErrSubscriptionProjection wrapping the projector's refusal", err)
			}
			fences := s.probe.take()
			if len(fences) != 1 || fences[0].err == nil {
				t.Fatalf("fences %+v, want the one fence, failed", fences)
			}
			after, _ := s.stored(t, res)
			if after.Version != fences[0].version {
				t.Fatalf("the failed fence left the version at %d, opened at %d", after.Version, fences[0].version)
			}
			if after.SubscriptionsSyncError != "" || !sameInstantPtr(after.SubscriptionsSyncedAt, *before.SubscriptionsSyncedAt) {
				t.Fatalf("the failed fence kept a stamp: error %q, synced %v (was %v)", after.SubscriptionsSyncError, after.SubscriptionsSyncedAt, before.SubscriptionsSyncedAt)
			}
		})
	}
}

// The short-circuit reaches the projection with no config to project from
// when the integration names a webhook that is gone. That is a refusal like
// any other, under the same policy — not a panic, and not a silent skip.
func TestProvision_ShortCircuit_MissingConfigIsARefusedProjection(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	if err := s.Webhooks.Delete(context.Background(), res.WebhookID); err != nil {
		t.Fatal(err)
	}
	s.projector.takeSyncs()
	s.hour()
	again := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	if again.Created {
		t.Fatal("the identical request took the rebuild path")
	}
	if syncs, _ := s.projector.takeSyncs(); len(syncs) != 0 {
		t.Fatalf("the projector was handed a config that does not exist: %+v", syncs[0].Config)
	}
	ri, err := s.Integrations.GetProvision(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ri.SubscriptionsSyncError, "does not exist") {
		t.Fatalf("the missing config is not recorded: subscriptions_sync_error = %q", ri.SubscriptionsSyncError)
	}
}

// ---- BotTriggers on the provision request ----

// A request that says nothing of bot_triggers keeps them; an entry replaces
// that one bot's value and leaves the others. Enabling one more bot is not a
// request to forget which labels start the first.
func TestProvision_BotTriggers_NilKeeps(t *testing.T) {
	s := newSpineOrch(t)
	nestor := BotTrigger{Labels: []string{"urgent"}, LaunchVars: map[string]string{"force": "true"}}
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}, BotTriggers: map[string]BotTrigger{"nestor": nestor}})
	want := map[string]BotTrigger{"nestor": nestor}
	check := func(stage string) {
		t.Helper()
		ri, _ := s.stored(t, res)
		if !botTriggersEqual(ri.BotTriggers, want) {
			t.Fatalf("%s: bot triggers = %+v, want %+v", stage, ri.BotTriggers, want)
		}
	}
	check("the first enable")

	// The short-circuit, silent about bot_triggers.
	if again := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}}); again.Created {
		t.Fatal("the identical request took the rebuild path")
	}
	check("a silent short-circuit")

	// The rebuild, silent about bot_triggers.
	s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "saboteur"}})
	check("a silent rebuild")

	// An entry for another bot: nestor's stays. Through the short-circuit…
	anyLabel := BotTrigger{Labels: []string{}}
	s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "saboteur"}, BotTriggers: map[string]BotTrigger{"saboteur": anyLabel}})
	want["saboteur"] = anyLabel
	check("an entry for another bot, short-circuit")
	ri, _ := s.stored(t, res)
	if l := ri.BotTriggers["saboteur"].Labels; l == nil {
		t.Fatal("the spoken-empty labels list (any label) was stored as nil (the manifest's)")
	}

	// …and through the rebuild, where the entry replaces the bot's whole value.
	off := false
	replaced := BotTrigger{Enabled: &off}
	s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "saboteur", "feature-dev"}, BotTriggers: map[string]BotTrigger{"saboteur": replaced}})
	want["saboteur"] = replaced
	// feature-dev is label-less and unpinned, but the integration was adopted
	// at its first projection: no pin is added for it.
	check("an entry replacing a bot's value, rebuild")
}

// A same-bot-set request carrying bot_triggers is a real mutation: the
// integration takes the entry and the projected row follows it. Without the
// short-circuit comparing it, the request answers 200 and changes nothing.
func TestProvision_ShortCircuit_BotTriggersChangesRows(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "saboteur"}})
	if got := s.projector.rowLabels(res.IntegrationID, "nestor"); !slices.Equal(got, []string{"nestor"}) {
		t.Fatalf("precondition: nestor's row claims %v, want its manifest label", got)
	}
	before, _ := s.stored(t, res)

	again := s.provision(t, ProvisionRequest{
		BotIDs:      []string{"nestor", "saboteur"},
		BotTriggers: map[string]BotTrigger{"nestor": {Labels: []string{"urgent", "p0"}}},
	})
	if again.Created {
		t.Fatal("the same bot set took the rebuild path, so this proves nothing about the short-circuit")
	}
	if got := s.projector.rowLabels(res.IntegrationID, "nestor"); !slices.Equal(got, []string{"urgent", "p0"}) {
		t.Fatalf("nestor's projected row still claims %v after a bot_triggers write of [urgent p0]", got)
	}
	if got := s.projector.rowLabels(res.IntegrationID, "saboteur"); !slices.Equal(got, []string{"saboteur"}) {
		t.Fatalf("saboteur's row moved to %v: the write named nestor only", got)
	}
	after, _ := s.stored(t, res)
	if !slices.Equal(after.BotTriggers["nestor"].Labels, []string{"urgent", "p0"}) {
		t.Fatalf("the integration does not hold the entry: %+v", after.BotTriggers)
	}
	// One whole-document write for the entry, one fence for the projection.
	if after.Version != before.Version+2 {
		t.Fatalf("version %d → %d, want the Update and the one fence", before.Version, after.Version)
	}
}

// ---- the routing switch and the projection stamps survive a rebuild ----

func TestProvision_CarriesForgeRoutingAcrossRebuild(t *testing.T) {
	// retireHookIdentity makes the next provision migrate to a NEW webhook
	// config (a Create, where nothing in the store can keep a field for the
	// provisioner): the integration loses its sealed hook secret, as a row
	// provisioned before sealed identities holds none.
	retireHookIdentity := func(t *testing.T, s *spineOrch, res ProvisionResult) {
		t.Helper()
		ri, _ := s.stored(t, res)
		ri.WebhookSecretSealed = nil
		if err := s.Integrations.Update(context.Background(), ri); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		migrate bool
	}{
		{name: "rebuild onto the same webhook config"},
		{name: "rebuild onto a new webhook config", migrate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpineOrch(t)
			res := s.provision(t, ProvisionRequest{
				BotIDs:      []string{"nestor"},
				BotTriggers: map[string]BotTrigger{"nestor": {Labels: []string{"urgent"}}},
			})
			adoptedAt := s.now()
			flippedAt := s.hour()
			s.flipToSpine(t, res)
			if tc.migrate {
				retireHookIdentity(t, s, res)
			}
			s.hour()
			s.projector.takeSyncs()

			// Enabling one more bot takes the rebuild path: both documents
			// are built again from literals.
			rebuilt := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "saboteur"}})
			if migrated := rebuilt.WebhookID != res.WebhookID; migrated != tc.migrate {
				t.Fatalf("webhook %q → %q: the row did not take the path it names", res.WebhookID, rebuilt.WebhookID)
			}
			ri, cfg := s.stored(t, rebuilt)

			if ri.ForgeRouting.Issues != webhooks.RoutingSpine {
				t.Errorf("the integration's routing was rebuilt to %+v: enabling a bot reverted the flip, with no error", ri.ForgeRouting)
			}
			if !sameInstantPtr(ri.SpineSince, flippedAt) {
				t.Errorf("the integration's spine_since = %v, want the flip instant %v", ri.SpineSince, flippedAt)
			}
			if cfg.ForgeRouting.Issues != webhooks.RoutingSpine {
				t.Errorf("the webhook config's routing was rebuilt to %+v: deliveries are back on the server default", cfg.ForgeRouting)
			}
			if !sameInstantPtr(cfg.SpineSince, flippedAt) {
				t.Errorf("the webhook config's spine_since = %v, want %v: legacy would stop reading the spine's claims after a rollback", cfg.SpineSince, flippedAt)
			}
			if !sameInstantPtr(ri.SubscriptionsAdoptedAt, adoptedAt) {
				t.Errorf("the adoption stamp = %v, want it kept at %v: the rebuild made the next projection adopt again", ri.SubscriptionsAdoptedAt, adoptedAt)
			}
			if !slices.Equal(ri.BotTriggers["nestor"].Labels, []string{"urgent"}) {
				t.Errorf("the operator's bot trigger was rebuilt away: %+v", ri.BotTriggers)
			}

			// The projection of that same provision read both documents as
			// rebuilt: it must see the flip too, or it projects — and applies
			// its error policy — as if the lane were on legacy.
			syncs, _ := s.projector.takeSyncs()
			if len(syncs) != 1 {
				t.Fatalf("%d projection(s) in the rebuild, want one", len(syncs))
			}
			if got := syncs[0].Config.ForgeRouting.Issues; got != webhooks.RoutingSpine {
				t.Errorf("the projection read a config routing %q", got)
			}
			if got := syncs[0].Integration.ForgeRouting.Issues; got != webhooks.RoutingSpine {
				t.Errorf("the projection read an integration routing %q", got)
			}
			if !sameInstantPtr(syncs[0].Config.SpineSince, flippedAt) {
				t.Errorf("the projection read a config spine_since %v", syncs[0].Config.SpineSince)
			}
		})
	}
}

// ---- the read-back after the short-circuit's two-store write ----

// lyingIntegrations answers Get with an integration altered by tamper: a
// store whose write reported success and lost to another writer.
type lyingIntegrations struct {
	RepoIntegrationStore
	tamper func(*RepoIntegration)
}

func (l *lyingIntegrations) Get(ctx context.Context, id string) (RepoIntegration, error) {
	ri, err := l.RepoIntegrationStore.Get(ctx, id)
	if err == nil && l.tamper != nil {
		l.tamper(&ri)
	}
	return ri, err
}

type lyingConfigs struct {
	webhooks.ConfigStore
	mu     sync.Mutex
	armed  bool
	tamper func(*webhooks.Config)
}

func (l *lyingConfigs) Update(ctx context.Context, c webhooks.Config) error {
	err := l.ConfigStore.Update(ctx, c)
	l.mu.Lock()
	l.armed = err == nil
	l.mu.Unlock()
	return err
}

func (l *lyingConfigs) Get(ctx context.Context, id string) (webhooks.Config, error) {
	c, err := l.ConfigStore.Get(ctx, id)
	l.mu.Lock()
	armed := l.armed
	l.mu.Unlock()
	if err == nil && armed && l.tamper != nil {
		l.tamper(&c)
	}
	return c, err
}

// verifyOperatorSettings refuses to certify a write that does not read back:
// the per-bot triggers on the integration, and the routing switch on either
// document. One row per compared field — each is its own guard.
func TestProvision_ShortCircuit_ReadBackComparesBotTriggersAndForgeRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		integration func(*RepoIntegration)
		config      func(*webhooks.Config)
		wantHalf    string
	}{
		{
			name:        "the integration lost the bot triggers",
			integration: func(ri *RepoIntegration) { ri.BotTriggers = nil },
			wantHalf:    "the integration does not read back",
		},
		{
			name:        "the integration's routing moved",
			integration: func(ri *RepoIntegration) { ri.ForgeRouting.Issues = webhooks.RoutingLegacy },
			wantHalf:    "the integration does not read back",
		},
		{
			name:     "the config's routing moved",
			config:   func(c *webhooks.Config) { c.ForgeRouting.Issues = webhooks.RoutingLegacy },
			wantHalf: "the webhook config does not read back",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpineOrch(t)
			res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
			s.flipToSpine(t, res)
			if tc.integration != nil {
				s.Integrations = &lyingIntegrations{RepoIntegrationStore: s.Integrations, tamper: tc.integration}
			}
			if tc.config != nil {
				s.Webhooks = &lyingConfigs{ConfigStore: s.Webhooks, tamper: tc.config}
			}
			_, err := s.Provision(context.Background(), ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"nestor"}, ActorID: "u1",
				BotTriggers: map[string]BotTrigger{"nestor": {Labels: []string{"urgent"}}},
			})
			if !errors.Is(err, ErrProvisionDiverged) || !errors.Is(err, ErrProvisionDivergedRetry) {
				t.Fatalf("a write that does not read back was certified: err = %v, want ErrProvisionDiverged (retry-convergent)", err)
			}
			if !strings.Contains(err.Error(), tc.wantHalf) {
				t.Fatalf("the refusal names the wrong store: %v", err)
			}
		})
	}
}

// ---- the parked-approval twin ----

// A provisioning parked for the org's decision replays its bot_triggers on
// approval. The record keeps what each entry spoke, through the codec the
// Mongo store encodes it with and on the org's wire: an empty labels list is
// the any-label gesture, and a record that folded it into nil would replay
// "follow the manifest" where the org approved "any label".
func TestProvisionApproval_BotTriggersKeepWhatEachEntrySpoke(t *testing.T) {
	off := false
	in := ProvisionApproval{
		ID: "a1", OrgID: "org", TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"nestor"},
		BotTriggers: map[string]BotTrigger{"nestor": {Labels: []string{}}, "saboteur": {Enabled: &off}},
	}
	check := func(codec string, out ProvisionApproval) {
		t.Helper()
		if !botTriggersEqual(out.BotTriggers, in.BotTriggers) {
			t.Fatalf("%s: the parked request's bot triggers = %+v, want %+v", codec, out.BotTriggers, in.BotTriggers)
		}
		if l := out.BotTriggers["nestor"].Labels; l == nil {
			t.Fatalf("%s: the spoken-empty labels list came back nil", codec)
		}
		if out.BotTriggers["saboteur"].Labels != nil {
			t.Fatalf("%s: a silent labels list came back as %#v", codec, out.BotTriggers["saboteur"].Labels)
		}
	}

	raw, err := bson.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromBSON ProvisionApproval
	if err := bson.Unmarshal(raw, &fromBSON); err != nil {
		t.Fatal(err)
	}
	check("bson", fromBSON)

	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON ProvisionApproval
	if err := json.Unmarshal(body, &fromJSON); err != nil {
		t.Fatal(err)
	}
	check("json", fromJSON)

	// A request that said nothing parks no key: the replay is silent too.
	silent, err := json.Marshal(ProvisionApproval{ID: "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(silent), "bot_triggers") {
		t.Fatalf("a request silent about bot_triggers parks the key: %s", silent)
	}
}
