package storetest

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"reflect"
	"sort"
	"testing"
	"time"
)

// TeamAliveRuns is shared by the local and cloud twins. Idle running work
// still owns its frozen pool; parked work must pass resume admission instead.
func TeamAliveRuns(t *testing.T, s store.RunStore) {
	t.Helper()
	reader := store.AsTeamAliveRunsReader(s)
	if reader == nil {
		t.Fatal("store has no TeamAliveRunsReader")
	}
	ctx := store.WithTenant(context.Background(), "cutover-team")
	statuses := []store.RunStatus{store.RunStatusQueued, store.RunStatusRunning, store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator, store.RunStatusFailedResumable, store.RunStatusFailed, store.RunStatusCancelled, store.RunStatusFinished}
	want := []string{}
	for _, tenant := range []string{"cutover-team", "foreign-team"} {
		tc := store.WithTenant(context.Background(), tenant)
		for _, status := range statuses {
			id := tenant + "-" + string(status)
			run, err := s.CreateRun(tc, id, "cutover", nil)
			if err != nil {
				t.Fatal(err)
			}
			run.TenantID = tenant
			run.Status = status
			if err = s.SaveRun(tc, run); err != nil {
				t.Fatal(err)
			}
			if tenant == "cutover-team" && (status == store.RunStatusQueued || status == store.RunStatusRunning) {
				want = append(want, id)
			}
		}
	}
	idle, err := s.CreateRun(ctx, "cutover-idle", "cutover", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	idle.TenantID = "cutover-team"
	idle.Status = store.RunStatusRunning
	idle.LLMIdleSince = &now
	if err = s.SaveRun(ctx, idle); err != nil {
		t.Fatal(err)
	}
	want = append(want, idle.ID)
	sort.Strings(want)
	for _, readCtx := range []context.Context{ctx, context.Background()} {
		got, err := reader.ListAliveRunsForTeam(readCtx, "cutover-team")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("alive runs = %v,%v; want %v (including idle, excluding parked/foreign)", got, err, want)
		}
	}
	if got, err := reader.ListAliveRunsForTeam(store.WithTenant(context.Background(), "foreign-team"), "cutover-team"); err == nil {
		t.Fatalf("contradictory scope returned %v", got)
	}
	if got, err := reader.ListAliveRunsForTeam(context.Background(), ""); err == nil {
		t.Fatalf("empty scope returned %v", got)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader.ListAliveRunsForTeam(cancelled, "cutover-team"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	if got, err := reader.ListAliveRunsForTeam(context.Background(), "empty-team"); err != nil || len(got) != 0 {
		t.Fatalf("empty team = %v,%v", got, err)
	}
}
