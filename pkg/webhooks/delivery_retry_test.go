package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Dropping retry_target or next_attempt_at from BSON loses retries at restart;
// exposing the target in JSON leaks comment/issue text and operator launch vars.
func TestDeliveryRetryTargetSurvivesStorageButStaysPrivate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	d := Delivery{ID: "d", NextAttemptAt: &now, RetryTarget: &DeferredTarget{BotID: "bot", IdemKey: "key", Vars: map[string]string{"question": "private text"}, ExpectedSHA: "sha"}, PublicBase: "https://iterion.example"}
	raw, err := bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got Delivery
	if err := bson.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.RetryTarget == nil || got.RetryTarget.Vars["question"] != "private text" || got.RetryTarget.ExpectedSHA != "sha" || got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(now) || got.PublicBase != d.PublicBase {
		t.Fatalf("lost retry data: %+v", got)
	}
	raw, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var public map[string]any
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatal(err)
	}
	if _, ok := public["RetryTarget"]; ok {
		t.Fatal("target exposed")
	}
	if _, ok := public["retry_target"]; ok {
		t.Fatal("target exposed")
	}
}

// Both twins must select only due failures without a run and honor the batch
// bound. Removing the RunID CAS predicate can recreate an already persisted run.
func TestDeliveryStore_RetryDueAndExistingRunClaim(t *testing.T) {
	onDeliveryStoreTwins(t, func(t *testing.T, st DeliveryStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Millisecond)
		seed := func(id, status, run string, next *time.Time, target bool) Delivery {
			d := Delivery{ID: id, IdempotencyKey: "key-" + id, Status: status, RunID: run, Attempts: 1, NextAttemptAt: next, ReceivedAt: now}
			if target {
				d.RetryTarget = &DeferredTarget{BotID: "bot", Vars: map[string]string{"v": "saved"}}
			}
			if err := st.Insert(ctx, d); err != nil {
				t.Fatal(err)
			}
			return d
		}
		earlier, later := now.Add(-time.Minute), now.Add(time.Minute)
		seed("due-a", StatusLaunchError, "", &earlier, true)
		seed("due-b", StatusLaunchError, "", &now, true)
		seed("later", StatusLaunchError, "", &later, true)
		seed("accepted", StatusAccepted, "", &earlier, true)
		seed("exhausted", StatusLaunchExhausted, "", &earlier, true)
		seed("legacy", StatusLaunchError, "", &earlier, false)
		seed("unscheduled", StatusLaunchError, "", nil, true)
		run := seed("run", StatusLaunchError, "already-produced", &earlier, true)
		rows, err := st.ListRetryDue(ctx, now, 1)
		if err != nil || len(rows) != 1 || rows[0].ID != "due-a" {
			t.Fatalf("bounded due query: %+v %v", rows, err)
		}
		rows, err = st.ListRetryDue(ctx, now, 10)
		if err != nil || len(rows) != 2 {
			t.Fatalf("eligible rows: %+v %v", rows, err)
		}
		run.Status, run.RunID = StatusAccepted, ""
		if claimed, err := st.ClaimFailedRetry(ctx, run, 1); err != nil || claimed {
			t.Fatalf("claimed existing run: %v %v", claimed, err)
		}
		got, err := st.Get(ctx, "run")
		if err != nil || got.RunID != "already-produced" {
			t.Fatalf("run link lost: %+v %v", got, err)
		}
	})
}

// A recent-window lookup, missing parent scope, or lost nanosecond receipt
// ordering lets a retry revive closed/superseded work (or suppress another PR).
func TestDeliveryStore_RetryObsoleteScopeAndOrder(t *testing.T) {
	onDeliveryStoreTwins(t, func(t *testing.T, st DeliveryStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Millisecond)
		d := Delivery{ID: "retry", TenantID: "team", WebhookID: "hook", ProjectPath: "org/repo", SubjectID: "comment:9", ParentSubjectID: "pr:7", BotID: "bot", ReceivedAt: now, ReceiptOrder: now.UnixNano(), RetryTarget: &DeferredTarget{BotID: "bot"}}
		if err := st.Insert(ctx, d); err != nil {
			t.Fatal(err)
		}
		check := func(supersede, want bool) {
			t.Helper()
			got, err := st.RetryObsolete(ctx, d, supersede)
			if err != nil || got != want {
				t.Fatalf("obsolete(supersede=%v)=%v, want %v: %v", supersede, got, want, err)
			}
		}
		other := d
		other.ID, other.IdempotencyKey = "outcome", "outcome"
		other.ReceiptOrder++ // Same BSON millisecond, but strictly newer.
		other.RetryTarget, other.ClosesSubject, other.SubjectID = nil, true, "pr:7"
		other.ProjectPath = "another/repo"
		if err := st.Insert(ctx, other); err != nil {
			t.Fatal(err)
		}
		check(true, false)
		for _, change := range []func(*Delivery){
			func(e *Delivery) { e.ProjectPath, e.TenantID = d.ProjectPath, "other-team" },
			func(e *Delivery) { e.TenantID, e.WebhookID = d.TenantID, "other-hook" },
			func(e *Delivery) { e.WebhookID, e.SubjectID = d.WebhookID, "pr:8" },
			func(e *Delivery) { e.SubjectID, e.ReceiptOrder = "pr:7", d.ReceiptOrder-1 },
		} {
			change(&other)
			if err := st.Update(ctx, other); err != nil {
				t.Fatal(err)
			}
			check(true, false)
		}
		other.ReceiptOrder = d.ReceiptOrder + 1
		if err := st.Update(ctx, other); err != nil {
			t.Fatal(err)
		}
		check(false, true) // A parent's closure applies regardless of overlap.
		other.ClosesSubject, other.SubjectID, other.RunID = false, d.SubjectID, "new-run"
		if err := st.Update(ctx, other); err != nil {
			t.Fatal(err)
		}
		check(false, false)
		check(true, true)
		other.RunID, other.RetryTarget = "", &DeferredTarget{BotID: d.BotID}
		if err := st.Update(ctx, other); err != nil {
			t.Fatal(err)
		}
		check(true, true) // The newer claim need not have finished launching.
		other.BotID = "another-bot"
		if err := st.Update(ctx, other); err != nil {
			t.Fatal(err)
		}
		check(true, false)
	})
}
