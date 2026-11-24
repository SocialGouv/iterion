package mongo

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
)

func prExplainCursor(raw bson.Raw) (bson.Raw, error) {
	if raw.Lookup("queryPlanner").Type != 0 {
		return raw, nil
	}
	stages, e := raw.Lookup("stages").Array().Values()
	if e != nil {
		return nil, e
	}
	for _, stage := range stages {
		if c := stage.Document().Lookup("$cursor"); c.Type != 0 {
			return c.Document(), nil
		}
	}
	return nil, fmt.Errorf("no explain cursor: %s", raw)
}
func TestRunPRActualBranchesUseTheirIndexes(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	var noise []any
	for n := range 600 {
		noise = append(noise, bson.M{"_id": fmt.Sprintf("noise-%d", n), "tenant_id": "format-tenant", "format_version": 99, "inputs": bson.M{"pr_url": fmt.Sprintf("other-%d", n)}, "inputs_pr_url": fmt.Sprintf("other-%d", n)})
	}
	for n := range 50 {
		noise = append(noise, bson.M{"_id": fmt.Sprintf("foreign-%d", n), "tenant_id": "another-tenant", "inputs": bson.M{"pr_url": "pr"}, "inputs_pr_url": "pr"})
	}
	noise = append(noise, bson.M{"_id": "deleted", "tenant_id": "format-tenant", "deleted_at": time.Now(), "format_version": 99, "inputs": bson.M{"pr_url": "pr"}, "inputs_pr_url": "pr"})
	if _, e := s.runs.InsertMany(ctx, noise); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"a", "b", "c"} {
		f := store.LegacyValueFormat
		if id == "b" {
			f = 2
		}
		if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, Format: f, Inputs: map[string]any{"pr_url": "pr"}}); e != nil {
			t.Fatal(e)
		}
	}
	// This legacy row predates the derived projection. Both index branches
	// are necessary; two other rows appear in both and must be deduplicated.
	if _, e := s.runs.UpdateOne(ctx, bson.M{"_id": "a"}, bson.M{"$unset": bson.M{"inputs_pr_url": ""}}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.runs.UpdateMany(ctx, bson.M{"_id": bson.M{"$in": bson.A{"a", "b", "c"}}}, bson.M{"$set": bson.M{"created_at": time.Unix(1, 0)}}); e != nil {
		t.Fatal(e)
	}
	var commands []bson.Raw
	var mu sync.Mutex
	reader := valueMonitoredClient(t, s, &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "aggregate" && e.Command.Lookup("aggregate").StringValue() == colRuns {
			mu.Lock()
			commands = append(commands, append(bson.Raw(nil), e.Command...))
			mu.Unlock()
		}
	}})
	for _, url := range []string{"pr", "absent"} {
		mu.Lock()
		commands = nil
		mu.Unlock()
		ids, e := reader.ListRunsForPR(ctx, url)
		if e != nil {
			t.Fatal(e)
		}
		if url == "pr" && !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
			t.Fatal(ids)
		}
		if url == "absent" && len(ids) != 0 {
			t.Fatal(ids)
		}
		mu.Lock()
		calls := append([]bson.Raw(nil), commands...)
		mu.Unlock()
		if len(calls) != 2 {
			t.Fatal("expected actual two indexed branches", len(calls))
		}
		for n, call := range calls {
			// Explain the captured production pipeline, without a hint or a test
			// reconstruction that could accidentally hide a full-scan implementation.
			aggregate := bson.D{{Key: "aggregate", Value: colRuns}, {Key: "pipeline", Value: call.Lookup("pipeline")}, {Key: "cursor", Value: bson.M{}}}
			raw, e := s.db.RunCommand(ctx, bson.D{{Key: "explain", Value: aggregate}, {Key: "verbosity", Value: "executionStats"}}).Raw()
			if e != nil {
				t.Fatal(e)
			}
			cursor, e := prExplainCursor(raw)
			if e != nil {
				t.Fatal(e)
			}
			plan := cursor.Lookup("queryPlanner", "winningPlan").String()
			index := "tenant_pr_url_created"
			if n == 1 {
				index = "tenant_exact_pr_url_created"
			}
			if strings.Contains(plan, "COLLSCAN") || !strings.Contains(plan, index) {
				t.Fatalf("branch%d chose %s", n, plan)
			}
			keys := cursor.Lookup("executionStats", "totalKeysExamined").AsInt64()
			docs := cursor.Lookup("executionStats", "totalDocsExamined").AsInt64()
			max := int64(3)
			if url == "absent" {
				max = 0
			}
			if keys > max || docs > max {
				t.Fatalf("branch%d %s examined %d keys/%d docs, max%d", n, url, keys, docs, max)
			}
			t.Logf("actual branch%d url=%s index=%s keys=%d docs=%d", n, url, index, keys, docs)
		}
	}
}
