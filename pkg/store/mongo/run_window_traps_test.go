package mongo

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// executedDocsExamined runs fn with the database profiler on and returns the
// documents the runs collection examined to answer it: the sum over every
// find and getMore fn sent, as mongod executed them. The plan cache is
// cleared first, so the plan is chosen for this question, not inherited from
// the previous one.
func executedDocsExamined(t *testing.T, s *Store, fn func()) int64 {
	t.Helper()
	return profiledDocsExamined(t, s, true, fn)
}

// warmDocsExamined is executedDocsExamined on the plan cache as fn finds it:
// what an earlier call of the same shape left is what fn's reads run, as in
// production, where every poller of every tenant shares mongod's plan cache.
func warmDocsExamined(t *testing.T, s *Store, fn func()) int64 {
	t.Helper()
	return profiledDocsExamined(t, s, false, fn)
}

func profiledDocsExamined(t *testing.T, s *Store, clearPlanCache bool, fn func()) int64 {
	t.Helper()
	ctx := context.Background()
	run := func(cmd bson.D) {
		t.Helper()
		if err := s.db.RunCommand(ctx, cmd).Err(); err != nil {
			t.Fatalf("%v: %v", cmd, err)
		}
	}
	if clearPlanCache {
		run(bson.D{{Key: "planCacheClear", Value: s.runs.Name()}})
	}
	run(bson.D{{Key: "profile", Value: 0}})
	if err := s.db.Collection("system.profile").Drop(ctx); err != nil {
		t.Fatalf("drop system.profile: %v", err)
	}
	run(bson.D{{Key: "profile", Value: 2}})
	fn()
	run(bson.D{{Key: "profile", Value: 0}})
	cur, err := s.db.Collection("system.profile").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"ns": s.db.Name() + "." + s.runs.Name(), "op": bson.M{"$in": bson.A{"query", "getmore"}}}},
		bson.M{"$group": bson.M{"_id": nil, "docs": bson.M{"$sum": "$docsExamined"}}},
	})
	if err != nil {
		t.Fatalf("read the profile: %v", err)
	}
	defer cur.Close(ctx)
	var out []struct {
		Docs int64 `bson:"docs"`
	}
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("decode the profile: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the profiler recorded no read of the runs collection — the measurement measured nothing")
	}
	return out[0].Docs
}

// TestListRunWindow_ExecutionReadsThePageNotTheCollection reproduces, at the
// counts that make the planner choose, the two shapes where a newest-first
// window chose created_id_desc and walked the whole collection for an answer
// of a few hundred rows (#3822 review): a team whose runs are dense among the
// newest, listed with no limit or a limit above the planner's trial batch;
// and a `since` band of a couple of hundred recently updated runs read at a
// limit above it. Planner choice follows counts, not document size, so the
// runs are small. Each read is bounded by what it answers, measured on the
// executed reads (profiler), not on one explain.
func TestListRunWindow_ExecutionReadsThePageNotTheCollection(t *testing.T) {
	s := watchersTestStore(t)
	ctx := context.Background()
	blind := store.TeamBlind(ctx)
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	const runs = 20000
	const fresh = 150
	start := end.Add(-runs * time.Minute)
	type seed struct {
		id      string
		tenant  string
		created time.Time
		updated time.Time
	}
	seeds := make([]seed, runs)
	for i := range runs {
		created := start.Add(time.Duration(i) * time.Minute)
		tenant := fmt.Sprintf("team-s%d", i%60)
		switch {
		case i >= runs-2*fresh && i%2 == 0:
			tenant = "team-fresh" // dense among the newest
		case i%4 != 0:
			tenant = "team-big"
		}
		seeds[i] = seed{
			id:      fmt.Sprintf("run-%05d", i),
			tenant:  tenant,
			created: created,
			// Within the hour after creation; the millisecond term keeps
			// every instant distinct, so `since` cuts where it is placed.
			updated: created.Add(time.Duration((i*7919)%3600)*time.Second + time.Duration(i)*time.Millisecond),
		}
	}
	// Resumes and late writes: a few hundred old runs updated over the last
	// day, so a recent `since` selects mostly the newest runs plus a handful
	// scattered over the whole history — the shape that lets the newest-first
	// index fill the planner's trial batch, then walk everything for the rest.
	for k := range 400 {
		i := (k * 7919) % (runs - 1000)
		seeds[i].updated = end.Add(-time.Duration(k) * 200 * time.Second)
	}
	docs := make([]any, 0, runs)
	for _, sd := range seeds {
		docs = append(docs, bson.M{
			"_id": sd.id, "tenant_id": sd.tenant, "status": "finished", "workflow_name": "wf",
			"created_at": sd.created, "updated_at": sd.updated,
		})
	}
	if _, err := s.runs.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The band: `since` keeps the M most recently updated runs, M between the
	// planner's trial batch (101) and the page's limit.
	updates := make([]time.Time, 0, runs)
	for _, sd := range seeds {
		updates = append(updates, sd.updated)
	}
	slices.SortFunc(updates, func(a, b time.Time) int { return b.Compare(a) })
	const page = 201
	since := updates[199-1]
	band := 0 // the runs `since` keeps, ties on its instant included
	for _, u := range updates {
		if !u.Before(since) {
			band++
		}
	}
	if band <= 101 || band > page {
		t.Fatalf("the band holds %d runs, want between the trial batch (101) and the limit (%d)", band, page)
	}

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		w       store.RunWindow
		rows    int
		maxDocs int64
	}{
		{"team dense among the newest, no limit", store.WithIdentity(ctx, "team-fresh", "owner"), store.RunWindow{}, fresh, fresh + 10},
		{"team dense among the newest, limit 202", store.WithIdentity(ctx, "team-fresh", "owner"), store.RunWindow{Limit: page + 1}, fresh, fresh + 10},
		{"recent since band, limit 202", blind, store.RunWindow{Since: since, Limit: page + 1}, band, int64(3 * (band + page))},
	} {
		var got []*store.Run
		docsExamined := executedDocsExamined(t, s, func() {
			var skipped []store.SkippedRun
			var err error
			got, skipped, err = s.ListRunWindow(tc.ctx, tc.w)
			if err != nil {
				t.Fatalf("%s: ListRunWindow: %v", tc.name, err)
			}
			if len(skipped) != 0 {
				t.Fatalf("%s: %d fixture runs did not decode (first: %v) — the bound would measure the refill, not the plan", tc.name, len(skipped), skipped[0].Err)
			}
		})
		if len(got) != tc.rows {
			t.Errorf("%s: listed %d runs, want %d", tc.name, len(got), tc.rows)
		}
		t.Logf("%s: %d rows, %d documents examined", tc.name, len(got), docsExamined)
		if docsExamined > tc.maxDocs {
			t.Errorf("%s: examined %d documents for %d rows, want at most %d — the read walked the collection, not the answer", tc.name, docsExamined, len(got), tc.maxDocs)
		}
	}
}

// TestListRunWindow_FilteredWindowsReadTheirMatchesColdAndWarm measures the
// team windows that filter on status or workflow (#3831) over thirty thousand
// runs: one team holding half of them, mid-sized and small teams, a team
// dense among the newest, and runs with no tenant. Each read is bounded by
// its answer, measured on the executed reads, with the plan cache cleared
// (cold) and as another call of the same shape left it (warm).
//
// A window that asks for the listing order with no limit ends the planner's
// trial at 101 results; an index that only hands back the order reaches them
// first among the team's newest runs and then walks the whole team for a
// status a narrower index holds in a few hundred keys. A cached plan is run
// for any tenant: the plan another team's window chose walked a team for its
// three hundred failed_resumable runs, and the batches of an admin team-set
// window ran the newest-first plan another set left. The indexes that hold a
// team's runs per status and per workflow in the listing order serve these
// windows whichever call left the cache, and only `since` windows read in
// batches.
func TestListRunWindow_FilteredWindowsReadTheirMatchesColdAndWarm(t *testing.T) {
	s := watchersTestStore(t)
	ctx := context.Background()
	blind := store.TeamBlind(ctx)
	team := func(id string) context.Context { return store.WithIdentity(ctx, id, "owner") }
	end := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	span := 200 * 24 * time.Hour
	rng := rand.New(rand.NewSource(3831))
	type seed struct {
		tenant, status, workflow string
		created                  time.Time
		deleted                  bool
	}
	var seeds []seed
	add := func(n int, tenant string, window time.Duration) {
		for range n {
			created := end.Add(-time.Duration(rng.Int63n(int64(window)))).Truncate(time.Millisecond)
			statuses := []string{"finished", "finished", "finished", "finished", "finished", "finished", "failed", "failed", "cancelled", "failed_resumable"}
			seeds = append(seeds, seed{
				tenant:   tenant,
				status:   statuses[rng.Intn(len(statuses))],
				workflow: fmt.Sprintf("wf-%d", rng.Intn(12)),
				created:  created,
				deleted:  rng.Intn(100) == 0,
			})
		}
	}
	add(15000, "team-big", span)
	add(3000, "team-mid", span)
	add(800, "team-newmid", 7*24*time.Hour)
	add(300, "team-fresh", 12*time.Hour) // dense among the newest
	for i := range 60 {
		add(150, fmt.Sprintf("team-s%d", i), span)
	}
	add(1900, "", span)
	docs := make([]any, 0, len(seeds))
	for i, sd := range seeds {
		doc := bson.M{
			"_id": fmt.Sprintf("run-%05d", i), "status": sd.status, "workflow_name": sd.workflow,
			"created_at": sd.created, "updated_at": sd.created.Add(time.Minute),
		}
		if sd.tenant != "" {
			doc["tenant_id"] = sd.tenant
		}
		if sd.deleted {
			doc["deleted_at"] = sd.created
		}
		docs = append(docs, doc)
	}
	if _, err := s.runs.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// matches counts the live runs a window answers, before its limit.
	matches := func(tenants []string, status, workflow string) int {
		n := 0
		for _, sd := range seeds {
			if !sd.deleted && slices.Contains(tenants, sd.tenant) && (status == "" || sd.status == status) && (workflow == "" || sd.workflow == workflow) {
				n++
			}
		}
		return n
	}

	type window struct {
		ctx context.Context
		w   store.RunWindow
	}
	list := func(name string, wd window) []*store.Run {
		t.Helper()
		got, skipped, err := s.ListRunWindow(wd.ctx, wd.w)
		if err != nil {
			t.Fatalf("%s: ListRunWindow: %v", name, err)
		}
		if len(skipped) != 0 {
			t.Fatalf("%s: %d fixture runs did not decode (first: %v)", name, len(skipped), skipped[0].Err)
		}
		return got
	}
	for _, tc := range []struct {
		name string
		// primer, when set, runs twice on a cleared plan cache before the
		// measured window, which then runs on the cache as it left it.
		primer *window
		window window
		rows   int
	}{
		{"team-big status=cancelled, no limit", nil,
			window{team("team-big"), store.RunWindow{Status: "cancelled"}}, matches([]string{"team-big"}, "cancelled", "")},
		{"team-mid status=failed, no limit", nil,
			window{team("team-mid"), store.RunWindow{Status: "failed"}}, matches([]string{"team-mid"}, "failed", "")},
		{"team-big workflow=wf-3, no limit", nil,
			window{team("team-big"), store.RunWindow{Workflow: "wf-3"}}, matches([]string{"team-big"}, "", "wf-3")},
		{"team-mid workflow=wf-3, limit 50", nil,
			window{team("team-mid"), store.RunWindow{Workflow: "wf-3", Limit: 50}}, 50},
		{"team-mid status=failed_resumable limit 500, after team-big's", &window{team("team-big"), store.RunWindow{Status: "failed_resumable", Limit: 500}},
			window{team("team-mid"), store.RunWindow{Status: "failed_resumable", Limit: 500}}, matches([]string{"team-mid"}, "failed_resumable", "")},
		{"team-s3 workflow=wf-3 no limit, after team-big's", &window{team("team-big"), store.RunWindow{Workflow: "wf-3"}},
			window{team("team-s3"), store.RunWindow{Workflow: "wf-3"}}, matches([]string{"team-s3"}, "", "wf-3")},
		{"admin team set mid+s3 limit 501, after fresh+newmid", &window{blind, store.RunWindow{TenantIDs: []string{"team-fresh", "team-newmid"}, Limit: 501}},
			window{blind, store.RunWindow{TenantIDs: []string{"team-mid", "team-s3"}, Limit: 501}}, 501},
	} {
		if tc.rows <= 0 || (tc.window.w.Limit > 0 && tc.rows > tc.window.w.Limit) {
			t.Fatalf("%s: the fixture answers %d rows — the case measures nothing", tc.name, tc.rows)
		}
		var docsExamined int64
		var got []*store.Run
		if tc.primer == nil {
			docsExamined = executedDocsExamined(t, s, func() { got = list(tc.name, tc.window) })
		} else {
			_ = executedDocsExamined(t, s, func() {
				list(tc.name+" (primer)", *tc.primer)
				list(tc.name+" (primer)", *tc.primer)
			})
			docsExamined = warmDocsExamined(t, s, func() { got = list(tc.name, tc.window) })
		}
		if len(got) != tc.rows {
			t.Errorf("%s: listed %d runs, want %d", tc.name, len(got), tc.rows)
		}
		// The matches plus the tombstones among them (one in a hundred).
		maxDocs := int64(tc.rows + tc.rows/20 + 10)
		t.Logf("%s: %d rows, %d documents examined", tc.name, len(got), docsExamined)
		if docsExamined > maxDocs {
			t.Errorf("%s: examined %d documents for %d rows, want at most %d — the read walked a team or the collection, not its answer", tc.name, docsExamined, len(got), maxDocs)
		}
	}
}

// TestListRunWindow_CrossTenantStatusWindowReadsInBatches measures the
// admin's status window, which no index serves in the listing order across
// tenants: an incident leaves 150 failures among the newest 300 runs of
// twenty thousand, and failures are rare (one in two hundred) before it.
// created_id_desc finds the trial's matches among the newest runs and wins;
// asked for more than the trial's 101 rows in one Find, it then walks the
// collection for the scattered rest, where status_created holds them in a
// few hundred keys. Read in batches of runWindowBatch, each Find stops at
// its own limit. Measured cold, then warm on the plan the cold read left.
func TestListRunWindow_CrossTenantStatusWindowReadsInBatches(t *testing.T) {
	s := watchersTestStore(t)
	ctx := context.Background()
	blind := store.TeamBlind(ctx)
	end := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewSource(7))
	const runs = 20000
	failed := 0
	docs := make([]any, 0, runs)
	for k := range runs {
		created := end.Add(-time.Duration(k) * 90 * time.Second)
		status := "finished"
		if (k < 300 && k%2 == 0) || rng.Intn(200) == 0 {
			status = "failed"
			failed++
		}
		tenant := fmt.Sprintf("team-%d", rng.Intn(40))
		if k%3 == 0 {
			tenant = "team-big"
		}
		docs = append(docs, bson.M{"_id": fmt.Sprintf("run-%06d", runs-k), "tenant_id": tenant, "status": status, "workflow_name": "wf", "created_at": created, "updated_at": created.Add(time.Minute)})
	}
	if _, err := s.runs.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The same question through a tenant set that names the tenant-less
	// runs: no partial tenant index holds them, so no index serves it either.
	everyTenant := []string{"team-big", ""}
	for i := range 40 {
		everyTenant = append(everyTenant, fmt.Sprintf("team-%d", i))
	}
	for _, c := range []struct {
		limit   int
		tenants []string
	}{{201, nil}, {501, nil}, {201, everyTenant}, {501, everyTenant}} {
		limit := c.limit
		w := store.RunWindow{Status: "failed", Limit: limit, TenantIDs: c.tenants}
		rows := min(limit, failed)
		if rows <= 101 {
			t.Fatalf("limit %d answers %d rows — the window must exceed the planner's trial", limit, rows)
		}
		maxDocs := int64(2*rows + 100)
		for _, warm := range []bool{false, true} {
			var got []*store.Run
			read := func() {
				var err error
				if got, _, err = s.ListRunWindow(blind, w); err != nil {
					t.Fatalf("limit %d: ListRunWindow: %v", limit, err)
				}
			}
			var docsExamined int64
			if warm {
				docsExamined = warmDocsExamined(t, s, read)
			} else {
				docsExamined = executedDocsExamined(t, s, read)
			}
			if len(got) != rows {
				t.Errorf("limit %d warm=%v: listed %d runs, want %d", limit, warm, len(got), rows)
			}
			t.Logf("blind status=failed limit %d tenant set %v warm=%v: %d rows, %d documents examined", limit, c.tenants != nil, warm, len(got), docsExamined)
			if docsExamined > maxDocs {
				t.Errorf("blind status=failed limit %d tenant set %v warm=%v: examined %d documents for %d rows, want at most %d — the read walked the collection, not its answer", limit, c.tenants != nil, warm, docsExamined, len(got), maxDocs)
			}
		}
	}
}
