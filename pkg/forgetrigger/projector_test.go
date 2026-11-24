package forgetrigger_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/forgetrigger"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

func testProjector(st trigger.SubscriptionStore) forgetrigger.Projector {
	return forgetrigger.Projector{
		Store: st,
		Roles: func(context.Context) forgetrigger.RoleBots { return stockRoles },
		Clock: func() time.Time { return planInstant },
	}
}

// The three bots of the demo repository: a label bot that speaks through its
// forge: block, and two that declare their own label.
func demoFeatureDev() manifestBot {
	return manifestBot{
		id: "feature-dev",
		fr: &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventIssueLabeled}},
		invs: []bundle.Invocation{{
			Kind:    bundle.InvocationKindCommand,
			ArgsVar: "task_text",
			Command: &bundle.InvocationCommand{Name: "feature", OpensMR: true},
		}},
	}
}

func demoNestor(force string) manifestBot {
	return manifestBot{
		id: "nestor",
		fr: &bundle.ForgeRequirements{Webhook: &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"tone": "dry"}}},
		invs: []bundle.Invocation{{
			Kind:        bundle.InvocationKindForge,
			ContextVars: map[string]string{"force": force},
			Forge:       &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{"nestor"}},
		}},
	}
}

func demoSaboteur() manifestBot {
	return labelBot("saboteur", bundle.ExecutionBoard, "saboteur", "*-chaos")
}

func demoScope() webhooks.Config {
	cfg := botScope("feature-dev", "gone", "nestor", "saboteur")
	cfg.LaunchVars = map[string]string{"tone": "dry"}
	return cfg
}

// demoInput is the integration as it stands.
func demoInput() forge.SubscriptionInput {
	return planInput(demoScope(), demoFeatureDev(), demoNestor("true"), demoSaboteur())
}

// staleInput is the integration as an earlier sync saw it: nestor's context
// var has another value, a bot since removed is listed, saboteur is not.
func staleInput() forge.SubscriptionInput {
	return planInput(demoScope(), demoFeatureDev(), demoNestor("false"), labelBot("gone", bundle.ExecutionDirect, "gone"))
}

func mustSync(t *testing.T, st trigger.SubscriptionStore, in forge.SubscriptionInput) forge.SubscriptionReport {
	t.Helper()
	report, err := testProjector(st).SyncIntegration(context.Background(), in)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return report
}

// storedRows returns every row of the tenant, by id.
func storedRows(t *testing.T, st trigger.SubscriptionStore, tenant string) []trigger.Subscription {
	t.Helper()
	rows, err := st.ListByTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("list the tenant's rows: %v", err)
	}
	slices.SortFunc(rows, func(a, b trigger.Subscription) int { return strings.Compare(a.ID, b.ID) })
	return rows
}

func rowIDsOf(rows []trigger.Subscription) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID+" ("+row.BotID+")")
	}
	return ids
}

// writeCounter counts the writes that reach a store. Syncs may call it from
// several goroutines.
type writeCounter struct {
	trigger.SubscriptionStore
	mu      sync.Mutex
	creates int
	updates int
	deletes int
}

func (c *writeCounter) Create(ctx context.Context, s trigger.Subscription) error {
	c.mu.Lock()
	c.creates++
	c.mu.Unlock()
	return c.SubscriptionStore.Create(ctx, s)
}

func (c *writeCounter) Update(ctx context.Context, s trigger.Subscription) error {
	c.mu.Lock()
	c.updates++
	c.mu.Unlock()
	return c.SubscriptionStore.Update(ctx, s)
}

func (c *writeCounter) Delete(ctx context.Context, id string) error {
	c.mu.Lock()
	c.deletes++
	c.mu.Unlock()
	return c.SubscriptionStore.Delete(ctx, id)
}

func (c *writeCounter) counts() (creates, updates, deletes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates, c.updates, c.deletes
}

func TestProjector_RowsLandBoundToTheIntegration(t *testing.T) {
	ctx := context.Background()
	st := trigger.NewMemorySubscriptionStore()
	in := demoInput()
	report := mustSync(t, st, in)

	bound, err := st.ListByForgeBinding(ctx, testTenant, testBinding)
	if err != nil {
		t.Fatalf("list by binding: %v", err)
	}
	byOrigin, err := st.ListByOrigin(ctx, testTenant, testOrigin)
	if err != nil {
		t.Fatalf("list by origin: %v", err)
	}
	if len(bound) != 3 || len(byOrigin) != 3 {
		t.Fatalf("binding %q selects %d rows and origin %q %d, want 3 and 3: %q", testBinding, len(bound), testOrigin, len(byOrigin), rowIDsOf(storedRows(t, st, testTenant)))
	}
	wantDigest := forgetrigger.ManifestDigest(in.Bots, in.InvByBot, in.FrByBot, stockRoles)
	if report.Digest != wantDigest {
		t.Errorf("the report's digest is %s; ManifestDigest of the inputs is %s", report.Digest, wantDigest)
	}
	type want struct {
		key     string
		mode    bundle.ExecutionMode
		labels  []string
		exclude []string
		argsVar string
		vars    map[string]string
	}
	wants := map[string]want{
		"feature-dev": {key: "legacy:issue_labeled", mode: bundle.ExecutionBoard, exclude: []string{"*-chaos", "nestor", "saboteur"}, argsVar: "task_text"},
		"nestor": {key: bundle.InvocationKey(in.InvByBot["nestor"], 0), mode: bundle.ExecutionDirect, labels: []string{"nestor"}, argsVar: "feature_prompt",
			vars: map[string]string{"force": "true", "tone": "dry"}},
		"saboteur": {key: bundle.InvocationKey(in.InvByBot["saboteur"], 0), mode: bundle.ExecutionBoard, labels: []string{"saboteur", "*-chaos"}, argsVar: "feature_prompt"},
	}
	for _, row := range bound {
		w, ok := wants[row.BotID]
		if !ok {
			t.Errorf("unexpected row %s of bot %s", row.ID, row.BotID)
			continue
		}
		delete(wants, row.BotID)
		if wantID := trigger.DerivedForgeSubscriptionID(testTenant, row.BotID, w.key, testIntegration); row.ID != wantID {
			t.Errorf("bot %s: id %s, want %s (key %q on integration %s)", row.BotID, row.ID, wantID, w.key, testIntegration)
		}
		if row.ForgeBinding != testBinding || row.Origin != testOrigin {
			t.Errorf("bot %s: binding %q origin %q, want %q and %q", row.BotID, row.ForgeBinding, row.Origin, testBinding, testOrigin)
		}
		if row.TenantID != testTenant || row.Repo != testRepo || row.Invocation != bundle.InvocationKindForge || !row.Enabled {
			t.Errorf("bot %s: tenant %q repo %q invocation %q enabled %v", row.BotID, row.TenantID, row.Repo, row.Invocation, row.Enabled)
		}
		m := row.Match
		if !slices.Equal(m.Sources, []trigger.Source{trigger.SourceForge}) || !slices.Equal(m.Kinds, []string{"issue_labeled"}) ||
			!slices.Equal(m.Actions, []string{"labeled"}) || !slices.Equal(m.Repos, []string{testRepo}) {
			t.Errorf("bot %s: matcher %+v does not select a label added to an issue of %s", row.BotID, m, testRepo)
		}
		if !sameList(m.AddedLabels, w.labels) || !sameList(m.ExcludeAddedLabels, w.exclude) {
			t.Errorf("bot %s: added_labels %q exclude %q, want %q and %q", row.BotID, m.AddedLabels, m.ExcludeAddedLabels, w.labels, w.exclude)
		}
		if row.Mode != w.mode || row.ArgsVar != w.argsVar || !row.OpensMR {
			t.Errorf("bot %s: mode %q args_var %q opens_mr %v, want %q %q true", row.BotID, row.Mode, row.ArgsVar, row.OpensMR, w.mode, w.argsVar)
		}
		if !reflect.DeepEqual(nilIfEmpty(row.Vars), nilIfEmpty(w.vars)) {
			t.Errorf("bot %s: vars %v, want %v", row.BotID, row.Vars, w.vars)
		}
		if row.ProjectionDigest != wantDigest {
			t.Errorf("bot %s: projection digest %q, want %q", row.BotID, row.ProjectionDigest, wantDigest)
		}
	}
	for bot := range wants {
		t.Errorf("no row bound to the integration for bot %s", bot)
	}

	// The rows answer the binding, and the bus never lists them.
	candidates, err := st.ListCandidates(ctx, labelEvent("nestor"))
	if err != nil {
		t.Fatalf("list bus candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("the bus lists %q as candidates of a forge event", rowIDsOf(candidates))
	}
	for added, want := range map[string]string{"nestor": "nestor", "run-chaos": "saboteur", "bug": "feature-dev"} {
		if got := strings.Join(matchedBots(t, st, added), " "); got != want {
			t.Errorf("adding %q starts [%s], want [%s]", added, got, want)
		}
	}
	wantEnabled := map[string]map[string][]string{"issues": {
		"feature-dev": {"*", "!*-chaos", "!nestor", "!saboteur"},
		"nestor":      {"nestor"},
		"saboteur":    {"saboteur", "*-chaos"},
	}}
	if !reflect.DeepEqual(report.Enabled, wantEnabled) {
		t.Errorf("report.enabled = %v, want %v", report.Enabled, wantEnabled)
	}
}

func TestProjector_OverlapLeavesStoredRowsUntouched(t *testing.T) {
	st := trigger.NewMemorySubscriptionStore()
	mustSync(t, st, demoInput())
	before := storedRows(t, st, testTenant)
	if len(before) != 3 {
		t.Fatalf("want three stored rows before the refused sync, got %d", len(before))
	}

	// nestor now declares a second invocation that may claim its first
	// one's label, and saboteur has left the integration: a plan that was
	// written would rewrite one row and delete another.
	nestor := demoNestor("false")
	nestor.invs = append(nestor.invs, labelInvocation(bundle.ExecutionBoard, "NESTOR", "urgent"))
	in := planInput(demoScope(), demoFeatureDev(), nestor)

	later := testProjector(st)
	later.Clock = func() time.Time { return planInstant.Add(time.Hour) }
	report, err := later.SyncIntegration(context.Background(), in)
	var refusal *forgetrigger.ProjectionError
	if !errors.As(err, &refusal) {
		t.Fatalf("sync error = %v, want the plan's *ProjectionError", err)
	}
	if refusal.Code != forgetrigger.ProjectionOverlappingRows || refusal.Bot != "nestor" || len(refusal.Rows) != 2 {
		t.Errorf("refusal = code %q bot %q rows %q, want overlapping_rows on nestor's two rows", refusal.Code, refusal.Bot, refusal.Rows)
	}
	if len(report.Overlaps) != 1 || !strings.HasPrefix(report.Overlaps[0], "nestor: ") {
		t.Errorf("the report returned beside the refusal names overlaps %q, want nestor's", report.Overlaps)
	}
	if after := storedRows(t, st, testTenant); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused projection changed the stored rows:\n before %+v\n after  %+v", before, after)
	}
}

func TestProjector_SecondSyncWritesNothing(t *testing.T) {
	st := &writeCounter{SubscriptionStore: trigger.NewMemorySubscriptionStore()}
	first := mustSync(t, st, demoInput())
	if creates, updates, deletes := st.counts(); creates != 3 || updates != 0 || deletes != 0 {
		t.Fatalf("the first sync made %d creates, %d updates, %d deletes; want 3, 0, 0", creates, updates, deletes)
	}
	before := storedRows(t, st, testTenant)

	later := testProjector(st)
	later.Clock = func() time.Time { return planInstant.Add(time.Hour) }
	second, err := later.SyncIntegration(context.Background(), demoInput())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if creates, updates, deletes := st.counts(); creates != 3 || updates != 0 || deletes != 0 {
		t.Errorf("syncing the same input again wrote: %d creates, %d updates, %d deletes in all; want 3, 0, 0", creates, updates, deletes)
	}
	if after := storedRows(t, st, testTenant); !reflect.DeepEqual(after, before) {
		t.Errorf("syncing the same input again moved the rows:\n before %+v\n after  %+v", before, after)
	}
	if !reflect.DeepEqual(second, first) {
		t.Errorf("the two syncs report differently:\n first  %+v\n second %+v", first, second)
	}
}

// syncOutcome is what one of two racing syncs returns.
type syncOutcome struct {
	report forge.SubscriptionReport
	err    error
}

// referenceRows is the state one sync of in leaves on a store seeded by seed.
func referenceRows(t *testing.T, seed func(trigger.SubscriptionStore), in forge.SubscriptionInput) []trigger.Subscription {
	t.Helper()
	st := trigger.NewMemorySubscriptionStore()
	seed(st)
	mustSync(t, st, in)
	return storedRows(t, st, testTenant)
}

// listGate holds a sync between the listing of its rows and its writes.
type listGate struct {
	trigger.SubscriptionStore
	listed  chan struct{}
	proceed chan struct{}
}

func newListGate(st trigger.SubscriptionStore) *listGate {
	return &listGate{SubscriptionStore: st, listed: make(chan struct{}), proceed: make(chan struct{})}
}

func (g *listGate) ListByOrigin(ctx context.Context, tenantID, origin string) ([]trigger.Subscription, error) {
	rows, err := g.SubscriptionStore.ListByOrigin(ctx, tenantID, origin)
	close(g.listed)
	<-g.proceed
	return rows, err
}

func TestProjector_Idempotent_TwoConcurrentSyncs(t *testing.T) {
	seeds := map[string]func(trigger.SubscriptionStore){
		"on an empty store": func(trigger.SubscriptionStore) {},
		// A stale store owes the sync one rewrite (nestor's vars), one
		// creation (saboteur) and one deletion (the bot that left).
		"on a stale store": func(st trigger.SubscriptionStore) { mustSync(t, st, staleInput()) },
	}
	for name, seed := range seeds {
		want := referenceRows(t, seed, demoInput())
		if len(want) != 3 {
			t.Fatalf("%s: the reference sync leaves %d rows, want 3", name, len(want))
		}

		t.Run(name+", both running free", func(t *testing.T) {
			st := trigger.NewMemorySubscriptionStore()
			seed(st)
			in := demoInput()
			start := make(chan struct{})
			outcomes := make(chan syncOutcome, 2)
			for range 2 {
				go func() {
					<-start
					report, err := testProjector(st).SyncIntegration(context.Background(), in)
					outcomes <- syncOutcome{report, err}
				}()
			}
			close(start)
			first, second := <-outcomes, <-outcomes
			if first.err != nil || second.err != nil {
				t.Fatalf("two concurrent syncs of one plan: %v / %v", first.err, second.err)
			}
			// The transfer warnings describe the transition THIS sync applied
			// (forgetrigger.ResidualTransfers reads the rows it found): a sync
			// that lists after the other wrote applies none and warns of none.
			// Everything else in the two reports is the plan's and must agree;
			// each sync's warnings are a subset of what one sync alone warns.
			ref := trigger.NewMemorySubscriptionStore()
			seed(ref)
			alone, err := testProjector(ref).SyncIntegration(context.Background(), in)
			if err != nil {
				t.Fatalf("the reference sync: %v", err)
			}
			for _, o := range []syncOutcome{first, second} {
				for _, w := range o.report.Warnings {
					if !slices.Contains(alone.Warnings, w) {
						t.Errorf("a concurrent sync warns %q, which one sync alone does not (%q)", w, alone.Warnings)
					}
				}
			}
			a, b := first.report, second.report
			a.Warnings, b.Warnings = nil, nil
			if !reflect.DeepEqual(a, b) {
				t.Errorf("the two syncs report differently:\n %+v\n %+v", first.report, second.report)
			}
			if got := storedRows(t, st, testTenant); !reflect.DeepEqual(got, want) {
				t.Errorf("two concurrent syncs leave\n %+v\nwhere one leaves\n %+v", got, want)
			}
		})

		// Both syncs list the same rows before either writes; then one
		// writes everything, then the other, in each of the two orders.
		for _, firstToWrite := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s, both listed, sync %d writes first", name, firstToWrite), func(t *testing.T) {
				st := trigger.NewMemorySubscriptionStore()
				seed(st)
				in := demoInput()
				gates := []*listGate{newListGate(st), newListGate(st)}
				outcomes := []chan syncOutcome{make(chan syncOutcome, 1), make(chan syncOutcome, 1)}
				for i, gate := range gates {
					go func() {
						report, err := testProjector(gate).SyncIntegration(context.Background(), in)
						outcomes[i] <- syncOutcome{report, err}
					}()
				}
				<-gates[0].listed
				<-gates[1].listed
				order := []int{firstToWrite, 1 - firstToWrite}
				for position, i := range order {
					close(gates[i].proceed)
					if out := <-outcomes[i]; out.err != nil {
						t.Fatalf("sync %d, writing in position %d from the listing both took: %v", i, position+1, out.err)
					}
				}
				if got := storedRows(t, st, testTenant); !reflect.DeepEqual(got, want) {
					t.Errorf("two interleaved syncs leave\n %+v\nwhere one leaves\n %+v", got, want)
				}
			})
		}
	}
}

func TestProjector_DeletesOnlyItsOrigin(t *testing.T) {
	ctx := context.Background()
	nestorKey := bundle.InvocationKey(demoNestor("true").invs, 0)
	seedForeign := func(t *testing.T, st trigger.SubscriptionStore) map[string]trigger.Subscription {
		t.Helper()
		foreign := []trigger.Subscription{
			{
				// An operator's own row against the integration's binding,
				// for a bot the integration lists.
				ID: "sub-operator-1", TenantID: testTenant, Repo: testRepo, BotID: "nestor",
				Invocation: bundle.InvocationKindForge, Origin: "operator", ForgeBinding: testBinding, Enabled: true,
				Match: trigger.Matcher{Sources: []trigger.Source{trigger.SourceForge}, AddedLabels: []string{"vip"}},
			},
			{
				// The same bot on another integration of the same repository.
				ID: trigger.DerivedForgeSubscriptionID(testTenant, "nestor", nestorKey, "int-2"), TenantID: testTenant, Repo: testRepo, BotID: "nestor",
				Invocation: bundle.InvocationKindForge, Origin: "forge:int-2", ForgeBinding: "integration:int-2", Enabled: true,
			},
			{
				// Another tenant's row under an origin that reads the same.
				ID: trigger.DerivedForgeSubscriptionID("team-2", "nestor", nestorKey, testIntegration), TenantID: "team-2", Repo: testRepo, BotID: "nestor",
				Invocation: bundle.InvocationKindForge, Origin: testOrigin, ForgeBinding: testBinding, Enabled: true,
			},
			{
				// A bus subscription of the same bot.
				ID: "sub-bus-1", TenantID: testTenant, BotID: "nestor",
				Invocation: bundle.InvocationKindBoard, Origin: "operator", Enabled: true,
			},
		}
		stored := map[string]trigger.Subscription{}
		for _, row := range foreign {
			if err := st.Create(ctx, row); err != nil {
				t.Fatalf("seed row %s: %v", row.ID, err)
			}
			kept, err := st.Get(ctx, row.ID)
			if err != nil {
				t.Fatalf("read seeded row %s: %v", row.ID, err)
			}
			stored[row.ID] = kept
		}
		return stored
	}
	assertForeignUntouched := func(t *testing.T, st trigger.SubscriptionStore, foreign map[string]trigger.Subscription, after string) {
		t.Helper()
		for id, want := range foreign {
			got, err := st.Get(ctx, id)
			if err != nil {
				t.Errorf("%s: row %s (origin %q, tenant %q) is gone: %v", after, id, want.Origin, want.TenantID, err)
				continue
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: row %s (origin %q) was rewritten:\n before %+v\n after  %+v", after, id, want.Origin, want, got)
			}
		}
	}

	t.Run("a sync removes the rows its plan dropped, of its origin only", func(t *testing.T) {
		st := trigger.NewMemorySubscriptionStore()
		mustSync(t, st, staleInput())
		foreign := seedForeign(t, st)
		goneID := trigger.DerivedForgeSubscriptionID(testTenant, "gone", bundle.InvocationKey(labelBot("gone", bundle.ExecutionDirect, "gone").invs, 0), testIntegration)
		if _, err := st.Get(ctx, goneID); err != nil {
			t.Fatalf("the stale sync did not leave the row of the bot that leaves: %v", err)
		}

		mustSync(t, st, demoInput())

		if _, err := st.Get(ctx, goneID); !errors.Is(err, trigger.ErrSubscriptionNotFound) {
			t.Errorf("the row of a bot the plan dropped is still stored (get: %v)", err)
		}
		assertForeignUntouched(t, st, foreign, "after a sync")
		own, err := st.ListByOrigin(ctx, testTenant, testOrigin)
		if err != nil || len(own) != 3 {
			t.Errorf("the integration holds %d rows of its origin (err %v), want its plan's 3", len(own), err)
		}
	})

	t.Run("a drop removes every row of its origin, and no other", func(t *testing.T) {
		st := trigger.NewMemorySubscriptionStore()
		mustSync(t, st, demoInput())
		foreign := seedForeign(t, st)

		if err := testProjector(st).DropIntegration(ctx, testTenant, testIntegration); err != nil {
			t.Fatalf("drop: %v", err)
		}

		own, err := st.ListByOrigin(ctx, testTenant, testOrigin)
		if err != nil || len(own) != 0 {
			t.Errorf("after a drop the integration still holds %q (err %v)", rowIDsOf(own), err)
		}
		assertForeignUntouched(t, st, foreign, "after a drop")
		// Dropping what is already gone is not a failure.
		if err := testProjector(st).DropIntegration(ctx, testTenant, testIntegration); err != nil {
			t.Errorf("a second drop: %v", err)
		}
	})
}

func TestProjector_RewritesProjectedFieldsAndKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	st := trigger.NewMemorySubscriptionStore()
	in := demoInput()
	mustSync(t, st, in)
	id := trigger.DerivedForgeSubscriptionID(testTenant, "nestor", bundle.InvocationKey(in.InvByBot["nestor"], 0), testIntegration)
	planned, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("read nestor's row: %v", err)
	}

	// The row is edited from outside the projection: its derived fields
	// (what the next sync owes back) and fields a projection does not own.
	edited := planned
	edited.Mode = bundle.ExecutionBoard
	edited.Match.AddedLabels = []string{"anything"}
	edited.Match.ExcludeAddedLabels = []string{"nestor"}
	edited.Vars = map[string]string{"force": "never"}
	edited.ArgsVar = "elsewhere"
	edited.OpensMR = false
	edited.Enabled = false
	edited.DisabledReason = "switched off by hand"
	edited.ProjectionDigest = "0000000000000000"
	edited.KeyOverrides = map[string]string{"anthropic": "key-1"}
	edited.CreatedBy = "ops@acme"
	if err := st.Update(ctx, edited); err != nil {
		t.Fatalf("edit nestor's row: %v", err)
	}
	if err := st.MarkRunOutcome(ctx, id, "run-1", "finished", "", "", planInstant.Add(time.Minute)); err != nil {
		t.Fatalf("stamp a run outcome: %v", err)
	}
	before, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("read the edited row: %v", err)
	}

	later := testProjector(st)
	resync := planInstant.Add(time.Hour)
	later.Clock = func() time.Time { return resync }
	if _, err := later.SyncIntegration(ctx, in); err != nil {
		t.Fatalf("resync: %v", err)
	}
	after, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("read the resynced row: %v", err)
	}

	// The projection's fields are the plan's again.
	want := planned
	if after.Mode != want.Mode || !reflect.DeepEqual(after.Match, want.Match) || !reflect.DeepEqual(after.Vars, want.Vars) ||
		after.ArgsVar != want.ArgsVar || after.OpensMR != want.OpensMR || after.Enabled != want.Enabled ||
		after.DisabledReason != want.DisabledReason || after.ProjectionDigest != want.ProjectionDigest ||
		after.ForgeBinding != want.ForgeBinding || after.Origin != want.Origin {
		t.Errorf("the resync did not restore the projected fields:\n planned %+v\n after   %+v", want, after)
	}
	// Everything else is the stored row's.
	if !reflect.DeepEqual(after.KeyOverrides, map[string]string{"anthropic": "key-1"}) || after.CreatedBy != "ops@acme" {
		t.Errorf("the resync dropped fields it does not own: key_overrides %v created_by %q", after.KeyOverrides, after.CreatedBy)
	}
	if !after.CreatedAt.Equal(planInstant) {
		t.Errorf("created_at moved to %v, want the first projection's %v", after.CreatedAt, planInstant)
	}
	if !after.UpdatedAt.Equal(resync) {
		t.Errorf("updated_at is %v, want the resync's %v", after.UpdatedAt, resync)
	}
	if after.Version != before.Version+1 {
		t.Errorf("version went from %d to %d, want one rewrite", before.Version, after.Version)
	}
	if after.LastRunID != "run-1" {
		t.Errorf("the resync lost the run outcome stamped on the row (last_run_id %q)", after.LastRunID)
	}
}

func TestProjector_RewritesARowThatDiffersByOneProjectedField(t *testing.T) {
	ctx := context.Background()
	in := demoInput()
	nestorID := trigger.DerivedForgeSubscriptionID(testTenant, "nestor", bundle.InvocationKey(in.InvByBot["nestor"], 0), testIntegration)
	edits := []struct {
		field string
		edit  func(row *trigger.Subscription)
	}{
		{"repo", func(row *trigger.Subscription) { row.Repo = "acme/renamed" }},
		{"bot_id", func(row *trigger.Subscription) { row.BotID = "someone-else" }},
		{"invocation", func(row *trigger.Subscription) { row.Invocation = bundle.InvocationKindBoard }},
		{"mode", func(row *trigger.Subscription) { row.Mode = bundle.ExecutionBoard }},
		{"match.added_labels", func(row *trigger.Subscription) { row.Match.AddedLabels = []string{"nestor", "more"} }},
		{"match.exclude_added_labels", func(row *trigger.Subscription) { row.Match.ExcludeAddedLabels = []string{"nestor"} }},
		{"match.repos", func(row *trigger.Subscription) { row.Match.Repos = nil }},
		{"args_var", func(row *trigger.Subscription) { row.ArgsVar = "elsewhere" }},
		{"vars", func(row *trigger.Subscription) { row.Vars = map[string]string{"force": "true"} }},
		{"opens_mr", func(row *trigger.Subscription) { row.OpensMR = false }},
		{"enabled", func(row *trigger.Subscription) { row.Enabled = false }},
		{"disabled_reason", func(row *trigger.Subscription) { row.DisabledReason = "left behind" }},
		{"projection_digest", func(row *trigger.Subscription) { row.ProjectionDigest = "0000000000000000" }},
	}
	for _, tc := range edits {
		t.Run(tc.field, func(t *testing.T) {
			st := trigger.NewMemorySubscriptionStore()
			mustSync(t, st, in)
			planned, err := st.Get(ctx, nestorID)
			if err != nil {
				t.Fatalf("read nestor's row: %v", err)
			}
			edited := planned
			edited.Match.AddedLabels = slices.Clone(planned.Match.AddedLabels)
			tc.edit(&edited)
			if err := st.Update(ctx, edited); err != nil {
				t.Fatalf("edit the row's %s: %v", tc.field, err)
			}
			if stored, _ := st.Get(ctx, nestorID); reflect.DeepEqual(projectedView(stored), projectedView(planned)) {
				t.Fatalf("the edit of %s did not reach the stored row", tc.field)
			}

			mustSync(t, st, in)

			after, err := st.Get(ctx, nestorID)
			if err != nil {
				t.Fatalf("read the resynced row: %v", err)
			}
			if !reflect.DeepEqual(projectedView(after), projectedView(planned)) {
				t.Errorf("a row whose %s alone differs from its plan is not rewritten:\n planned %+v\n after   %+v", tc.field, projectedView(planned), projectedView(after))
			}
		})
	}
}

// projectedView is a stored row without what a rewrite moves on its own.
func projectedView(row trigger.Subscription) trigger.Subscription {
	row.Version = 0
	row.UpdatedAt = time.Time{}
	return row
}

// unboundListing lists the integration's rows the way a replica that does
// not know the binding leaves them: without it.
type unboundListing struct {
	trigger.SubscriptionStore
	mu      sync.Mutex
	rewrote []trigger.Subscription
}

func (u *unboundListing) ListByOrigin(ctx context.Context, tenantID, origin string) ([]trigger.Subscription, error) {
	rows, err := u.SubscriptionStore.ListByOrigin(ctx, tenantID, origin)
	for i := range rows {
		rows[i].ForgeBinding = ""
	}
	return rows, err
}

func (u *unboundListing) Update(ctx context.Context, s trigger.Subscription) error {
	u.mu.Lock()
	u.rewrote = append(u.rewrote, s)
	u.mu.Unlock()
	return u.SubscriptionStore.Update(ctx, s)
}

func TestProjector_RestoresTheBindingOfARowThatLostIt(t *testing.T) {
	inner := trigger.NewMemorySubscriptionStore()
	mustSync(t, inner, demoInput())
	st := &unboundListing{SubscriptionStore: inner}

	mustSync(t, st, demoInput())

	st.mu.Lock()
	rewrote := slices.Clone(st.rewrote)
	st.mu.Unlock()
	if len(rewrote) != 3 {
		t.Fatalf("%d of the three rows listed without their binding were rewritten, want all of them", len(rewrote))
	}
	for _, row := range rewrote {
		if row.ForgeBinding != testBinding {
			t.Errorf("bot %s: the rewrite carries binding %q, want %q", row.BotID, row.ForgeBinding, testBinding)
		}
	}
}

// vanishingStore loses a row to another writer in the middle of the
// projector's write of it.
type vanishingStore struct {
	trigger.SubscriptionStore
	// rewriteAnswer is what the first Update is answered once its row is
	// gone; nil leaves Update alone.
	rewriteAnswer error
	// deleteAnswer is what every Delete is answered once its row is gone;
	// nil leaves Delete alone.
	deleteAnswer error
	mu           sync.Mutex
	vanished     bool
}

func (v *vanishingStore) Update(ctx context.Context, s trigger.Subscription) error {
	v.mu.Lock()
	first := v.rewriteAnswer != nil && !v.vanished
	v.vanished = v.vanished || first
	v.mu.Unlock()
	if !first {
		return v.SubscriptionStore.Update(ctx, s)
	}
	if err := v.SubscriptionStore.Delete(ctx, s.ID); err != nil {
		return err
	}
	return v.rewriteAnswer
}

func (v *vanishingStore) Delete(ctx context.Context, id string) error {
	if v.deleteAnswer == nil {
		return v.SubscriptionStore.Delete(ctx, id)
	}
	if err := v.SubscriptionStore.Delete(ctx, id); err != nil {
		return err
	}
	return v.deleteAnswer
}

func TestProjector_RowThatVanishesUnderTheWriteIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	seedStale := func(st trigger.SubscriptionStore) { mustSync(t, st, staleInput()) }
	want := referenceRows(t, seedStale, demoInput())

	for name, answer := range map[string]error{
		"a rewrite answered not-found creates the row again":             trigger.ErrSubscriptionNotFound,
		"a rewrite answered a conflict on a row now gone creates it too": trigger.ErrSubscriptionConflict,
	} {
		t.Run(name, func(t *testing.T) {
			inner := trigger.NewMemorySubscriptionStore()
			seedStale(inner)
			st := &vanishingStore{SubscriptionStore: inner, rewriteAnswer: answer}
			if _, err := testProjector(st).SyncIntegration(ctx, demoInput()); err != nil {
				t.Fatalf("sync: %v", err)
			}
			got := storedRows(t, inner, testTenant)
			for i := range got {
				got[i] = projectedView(got[i])
			}
			wantView := make([]trigger.Subscription, len(want))
			for i := range want {
				wantView[i] = projectedView(want[i])
			}
			if !reflect.DeepEqual(got, wantView) {
				t.Errorf("after a row vanished under its rewrite the store holds\n %+v\nwant the plan's rows\n %+v", got, wantView)
			}
		})
	}

	t.Run("a sync's delete of a row already gone", func(t *testing.T) {
		inner := trigger.NewMemorySubscriptionStore()
		seedStale(inner)
		st := &vanishingStore{SubscriptionStore: inner, deleteAnswer: trigger.ErrSubscriptionNotFound}
		if _, err := testProjector(st).SyncIntegration(ctx, demoInput()); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if got := storedRows(t, inner, testTenant); !reflect.DeepEqual(got, want) {
			t.Errorf("the store holds\n %+v\nwant\n %+v", got, want)
		}
	})

	t.Run("a drop's delete of a row already gone", func(t *testing.T) {
		inner := trigger.NewMemorySubscriptionStore()
		mustSync(t, inner, demoInput())
		st := &vanishingStore{SubscriptionStore: inner, deleteAnswer: trigger.ErrSubscriptionNotFound}
		if err := testProjector(st).DropIntegration(ctx, testTenant, testIntegration); err != nil {
			t.Fatalf("drop: %v", err)
		}
		if rows := storedRows(t, inner, testTenant); len(rows) != 0 {
			t.Errorf("after the drop the store still holds %q", rowIDsOf(rows))
		}
	})

	t.Run("a delete that fails for another reason is returned", func(t *testing.T) {
		broken := errors.New("store unreachable")
		inner := trigger.NewMemorySubscriptionStore()
		seedStale(inner)
		st := &vanishingStore{SubscriptionStore: inner, deleteAnswer: broken}
		if _, err := testProjector(st).SyncIntegration(ctx, demoInput()); !errors.Is(err, broken) {
			t.Errorf("sync error = %v, want the delete's", err)
		}
		if err := testProjector(st).DropIntegration(ctx, testTenant, testIntegration); !errors.Is(err, broken) {
			t.Errorf("drop error = %v, want the delete's", err)
		}
	})
}

// conflictingStore answers every rewrite with a version conflict, as a store
// whose rows another writer keeps moving.
type conflictingStore struct {
	trigger.SubscriptionStore
	mu      sync.Mutex
	updates int
}

func (c *conflictingStore) Update(context.Context, trigger.Subscription) error {
	c.mu.Lock()
	c.updates++
	c.mu.Unlock()
	return trigger.ErrSubscriptionConflict
}

func TestProjector_ConflictIsRetriedOnceThenReturned(t *testing.T) {
	inner := trigger.NewMemorySubscriptionStore()
	mustSync(t, inner, staleInput())
	before := storedRows(t, inner, testTenant)
	st := &conflictingStore{SubscriptionStore: inner}

	_, err := testProjector(st).SyncIntegration(context.Background(), demoInput())
	if !errors.Is(err, trigger.ErrSubscriptionConflict) {
		t.Fatalf("sync error = %v, want the version conflict the store keeps answering", err)
	}
	st.mu.Lock()
	updates := st.updates
	st.mu.Unlock()
	if updates != 2 {
		t.Errorf("the first row to rewrite was tried %d times, want 2: one write and one retry", updates)
	}
	if after := storedRows(t, inner, testTenant); !reflect.DeepEqual(after, before) {
		t.Errorf("a sync that failed on its first rewrite changed the stored rows:\n before %+v\n after  %+v", before, after)
	}
}

// racedStore loses every creation to another writer: the row a Create asks
// for is stored by someone else first, and the Create is refused as a
// duplicate — what a unique index answers.
type racedStore struct {
	trigger.SubscriptionStore
	// winner turns the row being created into the one the other writer
	// stored.
	winner func(trigger.Subscription) trigger.Subscription
}

var errDuplicateKey = errors.New("E11000 duplicate key error")

func (r *racedStore) Create(ctx context.Context, s trigger.Subscription) error {
	if err := r.SubscriptionStore.Create(ctx, r.winner(s)); err != nil {
		return err
	}
	return errDuplicateKey
}

func TestProjector_CreationThatLosesTheRaceReconcilesTheStandingRow(t *testing.T) {
	ctx := context.Background()
	want := referenceRows(t, func(trigger.SubscriptionStore) {}, demoInput())

	t.Run("the standing row is this integration's: it is reconciled", func(t *testing.T) {
		inner := trigger.NewMemorySubscriptionStore()
		st := &racedStore{SubscriptionStore: inner, winner: func(s trigger.Subscription) trigger.Subscription {
			// The other writer projected an earlier plan.
			s.Enabled = false
			s.DisabledReason = "projected from an earlier plan"
			s.ProjectionDigest = "0000000000000000"
			return s
		}}
		if _, err := testProjector(st).SyncIntegration(ctx, demoInput()); err != nil {
			t.Fatalf("sync: %v", err)
		}
		got := storedRows(t, inner, testTenant)
		if len(got) != len(want) {
			t.Fatalf("%d rows stored, want %d", len(got), len(want))
		}
		for i := range got {
			// The reconciliation is a rewrite: it moves the version.
			got[i].Version = want[i].Version
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the rows another writer created first are not reconciled to the plan:\n got  %+v\n want %+v", got, want)
		}
	})

	t.Run("the standing row is under another origin: it is refused and left alone", func(t *testing.T) {
		inner := trigger.NewMemorySubscriptionStore()
		st := &racedStore{SubscriptionStore: inner, winner: func(s trigger.Subscription) trigger.Subscription {
			s.Origin = "operator"
			s.Enabled = false
			return s
		}}
		_, err := testProjector(st).SyncIntegration(ctx, demoInput())
		if err == nil || !strings.Contains(err.Error(), `origin "operator"`) {
			t.Fatalf("sync error = %v, want a refusal naming the row's origin", err)
		}
		rows := storedRows(t, inner, testTenant)
		if len(rows) != 1 || rows[0].Origin != "operator" || rows[0].Enabled || rows[0].Version != 0 {
			t.Errorf("the row of another origin was touched, or the sync went on: %+v", rows)
		}
	})

	t.Run("no row stands: the creation's own error is returned", func(t *testing.T) {
		broken := errors.New("store unreachable")
		st := &failingCreates{SubscriptionStore: trigger.NewMemorySubscriptionStore(), err: broken}
		if _, err := testProjector(st).SyncIntegration(ctx, demoInput()); !errors.Is(err, broken) {
			t.Fatalf("sync error = %v, want the creation's error", err)
		}
	})
}

type failingCreates struct {
	trigger.SubscriptionStore
	err error
}

func (f *failingCreates) Create(context.Context, trigger.Subscription) error { return f.err }

// outcomeOf runs a projector call and reports a panic as a failure of the
// call, so a missing guard reads as a wrong answer.
func outcomeOf(call func() error) (panicked any, err error) {
	defer func() { panicked = recover() }()
	return nil, call()
}

func TestProjector_RefusesToRunUnwired(t *testing.T) {
	ctx := context.Background()
	roles := func(context.Context) forgetrigger.RoleBots { return stockRoles }
	// An integration whose bots claim no issue event plans no row: only the
	// projector's own guards stand between these calls and the store.
	noClaimer := func() forge.SubscriptionInput { return planInput(botScope("reviewer"), pullRequestBot("reviewer")) }
	unbound := noClaimer()
	unbound.Integration.ID = ""

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"a sync with no store", func() error {
			_, err := forgetrigger.Projector{Roles: roles}.SyncIntegration(ctx, noClaimer())
			return err
		}, "no subscription store"},
		{"a sync with no role-bot resolver", func() error {
			_, err := forgetrigger.Projector{Store: trigger.NewMemorySubscriptionStore()}.SyncIntegration(ctx, noClaimer())
			return err
		}, "no role-bot resolver"},
		{"a sync of an integration with no id", func() error {
			_, err := testProjector(trigger.NewMemorySubscriptionStore()).SyncIntegration(ctx, unbound)
			return err
		}, "carries no id"},
		{"a drop with no store", func() error {
			return forgetrigger.Projector{Roles: roles}.DropIntegration(ctx, testTenant, testIntegration)
		}, "no subscription store"},
		{"a drop of an integration with no id", func() error {
			return testProjector(trigger.NewMemorySubscriptionStore()).DropIntegration(ctx, testTenant, "")
		}, "carries no id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			panicked, err := outcomeOf(tc.call)
			if panicked != nil {
				t.Fatalf("the call panicked instead of refusing: %v", panicked)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}
