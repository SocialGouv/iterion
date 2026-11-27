package mongo

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestListRunWindow_ContinuationEdgesKeepEveryRun cuts a window at every
// position of a stream whose instants are shared by several runs, by a
// document refused by listingRecord and by a deleted run. Two edges are
// pinned. A document whose _id is an ObjectId or a number is skipped, yet it
// is the last one a Find saw when it ends a refill (the skipped documents ate
// the limit) or a `since` batch: the next Find starts from its id, and a
// query-language `_id: {$lt: <ObjectId>}` matches ObjectIds only, dropping
// every string-id run of that instant. And a run whose id starts with "$"
// is a caller's key like any other, never a field path. Each window, with and
// without `since` (which reads in batches of runWindowBatch), must equal the
// full stream cut after the caller's key.
func TestListRunWindow_ContinuationEdgesKeepEveryRun(t *testing.T) {
	s := watchersTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	const runs = 230
	var docs []any
	dollars := 0
	for i := range runs {
		c := base.Add(time.Duration(i/5) * time.Millisecond) // five runs per instant
		docs = append(docs, bson.M{"_id": fmt.Sprintf("run-%04d", i), "tenant_id": "team", "status": "finished", "workflow_name": "wf", "created_at": c, "updated_at": c})
		switch i % 23 {
		case 3:
			docs = append(docs, bson.M{"_id": fmt.Sprintf("run-%04d-sv", i), "tenant_id": "team", "status": "finished", "created_at": c, "updated_at": c, "v": SchemaVersion + 1})
		case 11:
			docs = append(docs, bson.M{"_id": fmt.Sprintf("run-%04d-del", i), "tenant_id": "team", "status": "finished", "created_at": c, "updated_at": c, "deleted_at": c})
		}
		if i%37 == 4 {
			// An ObjectId sorts above every string: first of its instant.
			docs = append(docs, bson.M{"_id": bson.NewObjectID(), "tenant_id": "team", "status": "finished", "created_at": c, "updated_at": c})
		}
		if i%29 == 7 {
			// String ids led by "$" read as field paths in an aggregation
			// expression unless they are literals; two per instant, so one
			// is the key of a window that must keep the other.
			for _, prefix := range []string{"$b", "$a"} {
				docs = append(docs, bson.M{"_id": fmt.Sprintf("%s-%04d", prefix, i), "tenant_id": "team", "status": "finished", "workflow_name": "wf", "created_at": c, "updated_at": c})
				dollars++
			}
		}
		if i%41 == 2 {
			// A number sorts below every string: last of its instant.
			docs = append(docs, bson.M{"_id": int64(i), "tenant_id": "team", "status": "finished", "created_at": c, "updated_at": c})
		}
	}
	if _, err := s.runs.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	since := base.Add(-time.Hour) // keeps every run, and reads in batches
	for _, scope := range []struct {
		name string
		ctx  context.Context
	}{{"team", store.WithIdentity(ctx, "team", "owner")}, {"blind", store.TeamBlind(ctx)}} {
		full, _, err := s.ListRunWindow(scope.ctx, store.RunWindow{})
		if err != nil {
			t.Fatalf("%s: full stream: %v", scope.name, err)
		}
		if len(full) != runs+dollars {
			t.Fatalf("%s: the full stream holds %d runs, want %d", scope.name, len(full), runs+dollars)
		}
		if !slices.IsSortedFunc(full, func(a, b *store.Run) int {
			if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(b.ID, a.ID)
		}) {
			t.Fatalf("%s: the full stream is not newest first, ids descending", scope.name)
		}
		failures := 0
		for p := -1; p < len(full) && failures < 5; p++ {
			var before store.RunKey
			if p >= 0 {
				before = store.RunKey{CreatedAt: full[p].CreatedAt, ID: full[p].ID}
			}
			rest := full[p+1:]
			for _, limit := range []int{1, 2, 3, 4, 99, 100, 101, 102, 150} {
				want := rest[:min(limit, len(rest))]
				for _, w := range []store.RunWindow{{Before: before, Limit: limit}, {Before: before, Limit: limit, Since: since}} {
					got, _, err := s.ListRunWindow(scope.ctx, w)
					if err != nil {
						t.Fatalf("%s %+v: %v", scope.name, w, err)
					}
					if !slices.Equal(windowIDs(got), windowIDs(want)) {
						failures++
						g, wt := windowIDs(got), windowIDs(want)
						at := 0
						for at < min(len(g), len(wt)) && g[at] == wt[at] {
							at++
						}
						t.Errorf("%s after position %d, limit %d, since %v: listed %d runs, want %d; from index %d got %v, want %v", scope.name, p, limit, !w.Since.IsZero(), len(g), len(wt), at, g[at:min(len(g), at+4)], wt[at:min(len(wt), at+4)])
					}
				}
			}
		}
	}
}

func windowIDs(rs []*store.Run) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}
