package mongo

import (
	"context"
	"fmt"
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
	ctx := context.Background()
	run := func(cmd bson.D) {
		t.Helper()
		if err := s.db.RunCommand(ctx, cmd).Err(); err != nil {
			t.Fatalf("%v: %v", cmd, err)
		}
	}
	run(bson.D{{Key: "planCacheClear", Value: s.runs.Name()}})
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
