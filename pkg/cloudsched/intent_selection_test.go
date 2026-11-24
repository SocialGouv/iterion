package cloudsched

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// The selection cap must not starve the schedules beyond it: with 201
// schedules carrying due intents and the lexical first 200 durably held
// (claimed, then finished on hold — their not_before keeps moving to a fresh
// retry instant), a lexical first-200 selection re-occupies the whole page at
// every tick and sched-200 is never attempted. The selection ranks by the
// schedule's OLDEST due intent instead, so the never-retried schedule wins
// the page. Both store twins, one contract.
func TestIntentSchedulesHeldPrefixDoesNotStarveMemory(t *testing.T) {
	assertHeldPrefixDoesNotStarve(t, NewMemoryStore())
}

func TestIntentSchedulesHeldPrefixDoesNotStarveMongo(t *testing.T) {
	assertHeldPrefixDoesNotStarve(t, NewMongoStore(scheduleMongo(t)))
}

func assertHeldPrefixDoesNotStarve(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 201; i++ {
		sb := ScheduledBot{ID: fmt.Sprintf("sched-%03d", i), BotID: "bot", Cron: "* * * * *", NextFireAt: base}
		if err := st.Create(ctx, sb); err != nil {
			t.Fatal(err)
		}
		if err := st.PutIntent(ctx, sb); err != nil {
			t.Fatal(err)
		}
	}
	// Hold the lexical prefix: each intent is claimed and finished on hold,
	// which moves its not_before to the retry instant. sched-200's intent is
	// never touched, so it keeps the oldest not_before of the page.
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("sched-%03d", i)
		rows, err := st.ClaimScheduleIntents(ctx, id, base, 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("claim %s: %v (%d rows)", id, err, len(rows))
		}
		if err := st.FinishIntent(ctx, rows[0], true, base); err != nil {
			t.Fatal(err)
		}
	}
	now := base.Add(intentRetry + time.Second)
	ids, err := st.ListIntentSchedules(ctx, now, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 200 {
		t.Fatalf("selection = %d schedules, want the 200 cap", len(ids))
	}
	if ids[0] != "sched-200" {
		t.Fatalf("oldest work first: ids[0] = %q, want sched-200 (never retried)", ids[0])
	}
	if !slices.Contains(ids, "sched-200") {
		t.Fatalf("the held lexical prefix starved sched-200 out of the selection: %v…", ids[:5])
	}
	if slices.Contains(ids, "sched-199") {
		// sched-199 shares the freshest not_before of the held prefix; the
		// id tie-break makes it the one schedule the cap leaves out.
		t.Fatalf("the cap must leave out the most recently retried schedule, not the oldest one: %v…", ids[:5])
	}
}
