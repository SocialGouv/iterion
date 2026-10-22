package mongo

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// watchersTestStore opens a throwaway database on the real replica set, or
// skips: the behaviour under test is the PLANNER's, which no filesystem port
// can stand in for.
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27017/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/ -run 'Plan'
func watchersTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the planner test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_watchers_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// planStats is what one explain says about a query: the plan, and how much
// it read.
type planStats struct {
	plan           string
	keysExamined   int64
	docsExamined   int64
	indexNames     []string
	collectionScan bool
	sortsInMemory  bool
}

// explainRunIDsBy explains the exact query listRunIDsBy sends for filter
// under ctx. The reply is read through the raw document: the client's
// registry decodes embedded documents as map[string]any, so a bson.M
// assertion on a nested field is false even when the field is there.
func explainRunIDsBy(t *testing.T, s *Store, ctx context.Context, filter bson.M) planStats {
	t.Helper()
	find := bson.D{
		{Key: "find", Value: s.runs.Name()},
		{Key: "filter", Value: notDeleted(withTenantFilter(ctx, filter))},
		{Key: "projection", Value: bson.M{"_id": 1}},
		{Key: "sort", Value: bson.D{{Key: "created_at", Value: 1}}},
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
	// SORT_MERGE merges index-ordered streams; a SORT stage would be the
	// in-memory sort of fetched documents.
	out.sortsInMemory = strings.Contains(out.plan, `"stage": "SORT"`)
	return out
}

// assertBoundedPlan is the shape every indexed question owes: one named
// index, no collection scan, no in-memory sort, and no more keys or
// documents read than the answer has rows (plus whatever slack the caller
// allows).
func assertBoundedPlan(t *testing.T, name string, st planStats, index string, rows int64) {
	t.Helper()
	if st.collectionScan {
		t.Errorf("%s: the winning plan scans the collection — the cost #2636 removed:\n%s", name, st.plan)
	}
	if st.sortsInMemory {
		t.Errorf("%s: the winning plan sorts documents in memory — the index was meant to hand the order back:\n%s", name, st.plan)
	}
	for _, idx := range st.indexNames {
		if idx != index {
			t.Errorf("%s: the winning plan scans %s, want %s alone:\n%s", name, idx, index, st.plan)
		}
	}
	if len(st.indexNames) == 0 {
		t.Errorf("%s: the winning plan scans no index:\n%s", name, st.plan)
	}
	// The expensive half is the document fetch: a partial index that holds
	// exactly the answer fetches the answer, nothing else.
	if st.docsExamined > rows || st.keysExamined > rows {
		t.Errorf("%s: the scan examined %d keys and %d documents for %d rows — it must read the answer alone", name, st.keysExamined, st.docsExamined, rows)
	}
}

// TestListDeliverableWatchers_PlanReadsTheWatchersAlone is #2636's acceptance
// on the real planner, over a population shaped like production: a thousand
// ended runs that once watched a card (WatchedIssueIDs is never cleared), a
// thousand that never did, three hundred live runs that watch nothing, a
// tombstoned watcher, an emptied one — and two live watchers. In the shape the
// cloud sweep sends it (unscoped) and in the tenant-scoped shape of an event,
// the query must run as an index scan over watchers_status_created, never a
// collection scan, never an in-memory sort, and must read no more documents
// than there are watchers: status_created alone serves the same query by
// fetching every LIVE run, three hundred whole documents for an answer of
// two, which is the residual this index removes. The population matters: at
// a few dozen documents the planner's choice between the two is noise.
func TestListDeliverableWatchers_PlanReadsTheWatchersAlone(t *testing.T) {
	s := watchersTestStore(t)
	tenant := store.WithIdentity(context.Background(), "tenant-plan", "owner-plan")
	fleet := store.WithoutTenantFilter(context.Background())

	create := func(id string, watched bool, status store.RunStatus) {
		t.Helper()
		if _, err := s.CreateRun(tenant, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if watched {
			if _, err := s.AddWatchedIssues(tenant, id, []string{"native:card-A"}); err != nil {
				t.Fatalf("AddWatchedIssues %s: %v", id, err)
			}
		}
		if err := s.UpdateRunStatus(tenant, id, status, ""); err != nil {
			t.Fatalf("UpdateRunStatus %s → %s: %v", id, status, err)
		}
	}
	const endedWatched, endedPlain, livePlain = 1000, 1000, 300
	for i := range endedWatched {
		create(fmt.Sprintf("ended-watched-%04d", i), true, store.RunStatusFinished)
	}
	for i := range endedPlain {
		create(fmt.Sprintf("ended-plain-%04d", i), false, store.RunStatusFinished)
	}
	for i := range livePlain {
		create(fmt.Sprintf("live-plain-%03d", i), false, store.RunStatusRunning)
	}
	create("watcher-running", true, store.RunStatusRunning)
	create("watcher-paused", true, store.RunStatusPausedWaitingHuman)
	create("emptied", true, store.RunStatusRunning)
	if _, err := s.RemoveWatchedIssues(tenant, "emptied", []string{"native:card-A"}); err != nil {
		t.Fatalf("RemoveWatchedIssues emptied: %v", err)
	}
	create("deleted", true, store.RunStatusRunning)
	if err := s.DeleteRun(tenant, "deleted"); err != nil {
		t.Fatalf("DeleteRun deleted: %v", err)
	}
	const watchers = 2

	for name, ctx := range map[string]context.Context{"fleet": fleet, "tenant": tenant} {
		got, err := s.ListDeliverableWatchers(ctx)
		if err != nil {
			t.Fatalf("ListDeliverableWatchers (%s): %v", name, err)
		}
		if len(got) != watchers || got[0] != "watcher-running" || got[1] != "watcher-paused" {
			t.Fatalf("ListDeliverableWatchers (%s) = %v, want [watcher-running watcher-paused] — ended, emptied, never-watched and tombstoned runs are not watchers", name, got)
		}
		assertBoundedPlan(t, name, explainRunIDsBy(t, s, ctx, deliverableWatchersFilter()), "watchers_status_created", watchers)
	}
}

// TestListRunsForPR_PlanReadsThePRsRunsAlone is the same acceptance for the
// reviewer→fixer hand-off's question, in the tenant-scoped shape it sends:
// over a tenant with twelve hundred runs on three hundred other pull requests,
// eight hundred runs with no pr_url input, three runs on the pull request
// asked for — and another tenant's three hundred runs on that very URL — the
// query must run as an index scan over tenant_pr_url_created and read the
// three rows alone. Without the index the planner falls back to
// tenant_status_created and reads every run of the tenant, plus an in-memory
// sort, once per webhook launch of a hand-off-consuming bot.
func TestListRunsForPR_PlanReadsThePRsRunsAlone(t *testing.T) {
	s := watchersTestStore(t)
	t1 := store.WithIdentity(context.Background(), "tenant-one", "owner-one")
	t2 := store.WithIdentity(context.Background(), "tenant-two", "owner-two")
	const target = "https://github.com/SocialGouv/iterion/pull/2636"
	n := 0
	create := func(ctx context.Context, inputs map[string]any) string {
		t.Helper()
		n++
		id := fmt.Sprintf("r%05d", n)
		if _, err := s.CreateRun(ctx, id, "review-pr", inputs); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		return id
	}
	for i := range 1200 {
		create(t1, map[string]any{"pr_url": fmt.Sprintf("https://github.com/SocialGouv/iterion/pull/%d", i%300), "base_ref": "main"})
	}
	for range 800 {
		create(t1, map[string]any{"base_ref": "main"})
	}
	var want []string
	for range 3 {
		want = append(want, create(t1, map[string]any{"pr_url": target}))
	}
	for range 300 {
		create(t2, map[string]any{"pr_url": target})
	}

	handoff := store.WithTenant(context.Background(), "tenant-one")
	got, err := s.ListRunsForPR(handoff, target)
	if err != nil {
		t.Fatalf("ListRunsForPR: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListRunsForPR = %v, want %v — this tenant's runs on this pull request, created_at ascending", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListRunsForPR = %v, want %v (created_at ascending)", got, want)
		}
	}
	assertBoundedPlan(t, "handoff", explainRunIDsBy(t, s, handoff, bson.M{"inputs.pr_url": target}), "tenant_pr_url_created", int64(len(want)))
}
