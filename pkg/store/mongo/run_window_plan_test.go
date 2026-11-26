package mongo

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// explainRunWindow explains the exact Find ListRunWindow sends for w from the
// top (or from w.Before) at limit, under ctx.
func explainRunWindow(t *testing.T, s *Store, ctx context.Context, w store.RunWindow, limit int) planStats {
	t.Helper()
	filter, _ := runWindowQuery(ctx, w, keyBound(w.Before), limit)
	return explainRunsFind(t, s, ctx, filter, listingProjection(), runWindowSort, limit)
}

// explainRunsFind explains one find on the runs collection and reads the
// plan the way explainRunIDsBy does; usedDisk is read off the execution
// stages, where a spilling sort reports it.
func explainRunsFind(t *testing.T, s *Store, ctx context.Context, filter, projection bson.M, sort bson.D, limit int) planStats {
	t.Helper()
	find := bson.D{
		{Key: "find", Value: s.runs.Name()},
		{Key: "filter", Value: filter},
		{Key: "projection", Value: projection},
		{Key: "sort", Value: sort},
	}
	if limit > 0 {
		find = append(find, bson.E{Key: "limit", Value: int64(limit)})
	}
	raw, err := s.db.RunCommand(ctx, bson.D{{Key: "explain", Value: find}, {Key: "verbosity", Value: "executionStats"}}).Raw()
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	winning, err := raw.LookupErr("queryPlanner", "winningPlan")
	if err != nil {
		t.Fatalf("explain reply carries no queryPlanner.winningPlan: %v\n%s", err, raw)
	}
	out := planStats{plan: winning.String()}
	for _, field := range []struct {
		name string
		dst  *int64
	}{{"totalKeysExamined", &out.keysExamined}, {"totalDocsExamined", &out.docsExamined}} {
		v, err := raw.LookupErr("executionStats", field.name)
		if err != nil {
			t.Fatalf("explain reply carries no executionStats.%s: %v", field.name, err)
		}
		n, ok := v.AsInt64OK()
		if !ok {
			t.Fatalf("executionStats.%s = %s, not a number", field.name, v)
		}
		*field.dst = n
	}
	for _, part := range strings.Split(out.plan, `"indexName": "`)[1:] {
		out.indexNames = append(out.indexNames, part[:strings.IndexByte(part, '"')])
	}
	out.collectionScan = strings.Contains(out.plan, `"COLLSCAN"`)
	out.sortsInMemory = strings.Contains(out.plan, `"stage": "SORT"`)
	if stages, err := raw.LookupErr("executionStats", "executionStages"); err == nil && strings.Contains(stages.String(), `"usedDisk": true`) {
		t.Errorf("the execution spilled to disk:\n%s", stages)
	}
	return out
}

// TestListRunWindow_PlanServesThePageFromAnIndex is #3822's acceptance on the
// real planner. Over three thousand runs of five teams, the questions the run
// listings ask must each be one indexed read: the cross-tenant newest-first
// page and its cursored next page walk created_id_desc with no sort stage and
// read about the page, not the collection; the `since` page reads an index,
// never the collection; a team's page walks tenant_created_id_desc the same
// way; a team-set page reads an index. ListRuns, which still lists
// everything, gets its unscoped order from an index instead of an in-memory
// sort of the whole collection. The execution bounds over the shapes that
// trap the planner are in run_window_traps_test.go.
func TestListRunWindow_PlanServesThePageFromAnIndex(t *testing.T) {
	s := watchersTestStore(t)
	ctx := context.Background()
	blind := store.TeamBlind(ctx)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const runs = 3000
	docs := make([]any, 0, runs)
	statuses := []string{"finished", "failed", "running", "queued", "cancelled"}
	for i := range runs {
		doc := bson.M{
			"_id":           fmt.Sprintf("run-%05d", i),
			"tenant_id":     fmt.Sprintf("team-%d", i%5),
			"status":        statuses[i%len(statuses)],
			"workflow_name": fmt.Sprintf("wf-%d", i%7),
			"created_at":    base.Add(time.Duration(i) * time.Minute),
			// Updated in an order unrelated to creation, so `since`
			// selects a scattered tenth of the runs.
			"updated_at": base.Add(time.Duration((i*7919)%runs) * time.Minute),
		}
		if i%97 == 0 {
			doc["deleted_at"] = base
		}
		docs = append(docs, doc)
	}
	if _, err := s.runs.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	since := base.Add(time.Duration(runs*9/10) * time.Minute)

	const page = 51
	assertIndexedPage := func(name string, st planStats, index string, sortFree bool, maxDocs int64) {
		t.Helper()
		if st.collectionScan {
			t.Errorf("%s: the winning plan scans the collection:\n%s", name, st.plan)
		}
		if len(st.indexNames) == 0 {
			t.Errorf("%s: the winning plan reads no index:\n%s", name, st.plan)
		}
		if index != "" && (len(st.indexNames) != 1 || st.indexNames[0] != index) {
			t.Errorf("%s: the winning plan reads %v, want %s alone:\n%s", name, st.indexNames, index, st.plan)
		}
		if sortFree && st.sortsInMemory {
			t.Errorf("%s: the winning plan sorts in memory — the index was meant to hand the order back:\n%s", name, st.plan)
		}
		if maxDocs > 0 && (st.docsExamined > maxDocs || st.keysExamined > maxDocs) {
			t.Errorf("%s: examined %d keys and %d documents, want at most %d — the page, not the collection", name, st.keysExamined, st.docsExamined, maxDocs)
		}
	}
	// Each read examines its page plus the tombstones it walks past.
	slack := int64(page + page/10 + 2)
	assertIndexedPage("admin first page", explainRunWindow(t, s, blind, store.RunWindow{}, page), "created_id_desc", true, slack)
	assertIndexedPage("admin cursored page", explainRunWindow(t, s, blind,
		store.RunWindow{Before: store.RunKey{CreatedAt: base.Add(2000 * time.Minute), ID: "run-02000"}}, page), "created_id_desc", true, slack)
	assertIndexedPage("admin since page", explainRunWindow(t, s, blind, store.RunWindow{Since: since}, 201), "", false, 0)
	assertIndexedPage("team page", explainRunWindow(t, s, store.WithIdentity(ctx, "team-3", "owner"), store.RunWindow{}, page), "tenant_created_id_desc", true, slack)
	assertIndexedPage("admin team-set page", explainRunWindow(t, s, blind, store.RunWindow{TenantIDs: []string{"team-1", "team-4"}}, page), "", false, 0)

	// The pages answer what they were asked, in order.
	got, skipped, err := s.ListRunWindow(blind, store.RunWindow{Since: since, Limit: 201})
	if err == nil && len(got) == 0 && len(skipped) == 0 {
		t.Errorf("the since page matched nothing — the plan above explained an empty question")
	}

	st := explainRunsFind(t, s, blind, notDeleted(withTenantFilter(blind, bson.M{})), bson.M{"_id": 1}, bson.D{{Key: "created_at", Value: 1}}, 0)
	assertIndexedPage("ListRuns", st, "created_id_desc", true, 0)
}
