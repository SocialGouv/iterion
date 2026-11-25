package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A carded row is the audit of a target the spine routed to a board card: the
// card is the claim and the board launches its run. The row itself must
// therefore never be handed out as work nor counted as a launch — whatever
// else it carries. Each store query that could do either gets its own row,
// built to satisfy every OTHER clause of that query, beside a control row
// that differs only by its status and IS selected: the status is then the
// one thing keeping the carded row out.
func TestDeliveryCarded_NeverClaimedRetriedOrCounted(t *testing.T) {
	onDeliveryStoreTwins(t, func(t *testing.T, st DeliveryStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
		due := now.Add(-time.Minute)
		row := func(id, status string) Delivery {
			return Delivery{
				ID: id, TenantID: "t1", WebhookID: "w1", Provider: ProviderGitHub,
				IdempotencyKey: "key-" + id, Status: status, BotID: "saboteur",
				EventKind: "issues", EventAction: "labeled", ProjectPath: "acme/api", SubjectID: "issue:7",
				SubscriptionID: "sub-saboteur", RoutedBy: RoutingSpine,
				Attempts: 1, ReceivedAt: now,
			}
		}
		insert := func(t *testing.T, d Delivery) {
			t.Helper()
			if err := st.Insert(ctx, d); err != nil {
				t.Fatalf("Insert %s: %v", d.ID, err)
			}
		}

		// Retried. Both rows are due, run-less and carry a parked target.
		retryable := func(id, status string) Delivery {
			d := row(id, status)
			d.NextAttemptAt = &due
			d.RetryTarget = &DeferredTarget{BotID: "saboteur", IdemKey: d.IdempotencyKey, SubscriptionID: "sub-saboteur", RoutedBy: RoutingSpine}
			return d
		}
		cardedDue := retryable("carded-due", StatusCarded)
		cardedDue.CardID = "card-7"
		insert(t, cardedDue)
		insert(t, retryable("failed-due", StatusLaunchError))
		rows, err := st.ListRetryDue(ctx, now, 10)
		if err != nil {
			t.Fatalf("ListRetryDue: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != "failed-due" {
			t.Fatalf("ListRetryDue = %v, want the failed row alone: a carded row handed to the retry sweeper launches a run its card already owns", deliveryIDs(rows))
		}

		// Claimed. The take-over that lets one caller relaunch a failed row
		// must refuse a carded one and leave it as it was.
		takeover := cardedDue
		takeover.Status, takeover.Attempts, takeover.CardID = StatusAccepted, 2, ""
		claimed, err := st.ClaimFailedRetry(ctx, takeover, 1)
		if err != nil {
			t.Fatalf("ClaimFailedRetry (carded): %v", err)
		}
		if claimed {
			t.Fatal("ClaimFailedRetry took over a carded row: the caller goes on to launch it")
		}
		kept, err := st.Get(ctx, "carded-due")
		if err != nil {
			t.Fatalf("Get carded-due: %v", err)
		}
		if kept.Status != StatusCarded || kept.CardID != "card-7" || kept.Attempts != 1 {
			t.Fatalf("the refused take-over rewrote the carded row: %+v", kept)
		}
		control := retryable("failed-due", StatusLaunchError)
		control.Status, control.Attempts = StatusAccepted, 2
		if claimed, err := st.ClaimFailedRetry(ctx, control, 1); err != nil || !claimed {
			t.Fatalf("ClaimFailedRetry (control, a failed row of the same shape) = (%v, %v), want (true, nil)", claimed, err)
		}

		// Counted. Both rows name a run on the same subject; only the launch
		// counts toward a ceiling.
		cardedRun := row("carded-run", StatusCarded)
		cardedRun.CardID, cardedRun.RunID = "card-8", "run-of-the-card"
		insert(t, cardedRun)
		launched := row("launched-run", StatusLaunched)
		launched.RunID = "run-direct"
		insert(t, launched)
		n, err := st.CountLaunched(ctx, "t1", "w1", "issues", "acme/api", "issue:7")
		if err != nil {
			t.Fatalf("CountLaunched: %v", err)
		}
		if n != 1 {
			t.Fatalf("CountLaunched = %d, want 1 (the launched row): a carded row counted as a launch", n)
		}

		// Audit-only is not invisible: the audit reads return the row with its
		// card and its attribution.
		byKey, err := st.GetByIdempotencyKey(ctx, "key-carded-run")
		if err != nil {
			t.Fatalf("GetByIdempotencyKey (carded): %v", err)
		}
		if byKey.Status != StatusCarded || byKey.CardID != "card-8" || byKey.SubscriptionID != "sub-saboteur" || byKey.RoutedBy != RoutingSpine {
			t.Fatalf("the carded audit row read back %+v, want status carded / card-8 / sub-saboteur / spine", byKey)
		}
		audit, err := st.ListByWebhook(ctx, "t1", "w1", 50)
		if err != nil {
			t.Fatalf("ListByWebhook: %v", err)
		}
		listed := map[string]string{}
		for _, d := range audit {
			listed[d.ID] = d.Status
		}
		for _, id := range []string{"carded-due", "carded-run"} {
			if listed[id] != StatusCarded {
				t.Errorf("the audit list reads %s as %q, want %q: a carded target must stay visible", id, listed[id], StatusCarded)
			}
		}
	})
}

func deliveryIDs(rows []Delivery) []string {
	ids := make([]string, 0, len(rows))
	for _, d := range rows {
		ids = append(ids, d.ID)
	}
	return ids
}

// The spine attribution is stored under exact keys and only when set: a row
// legacy routed encodes none of them, in the store and in the audit API, and
// a spine row gives its attribution back — on the row and on the parked
// target a retry rebuilds the launch from.
func TestDeliveryCodecs_SpineAttribution(t *testing.T) {
	legacy := Delivery{
		ID: "d-legacy", IdempotencyKey: "k", Status: StatusLaunchError, BotID: "feature-dev",
		RetryTarget: &DeferredTarget{BotID: "feature-dev", IdemKey: "k"},
	}
	spine := legacy
	spine.ID, spine.Status = "d-spine", StatusCarded
	spine.SubscriptionID, spine.RoutedBy, spine.CardID = "sub-1", RoutingSpine, "card-1"
	spine.RetryTarget = &DeferredTarget{BotID: "feature-dev", IdemKey: "k", SubscriptionID: "sub-1", RoutedBy: RoutingSpine}

	t.Run("bson", func(t *testing.T) {
		raw, err := bson.Marshal(legacy)
		if err != nil {
			t.Fatalf("bson.Marshal: %v", err)
		}
		for _, path := range [][]string{
			{"subscription_id"}, {"routed_by"}, {"card_id"},
			{"retry_target", "subscription_id"}, {"retry_target", "routed_by"},
		} {
			if v, ok := bsonField(t, raw, path...); ok {
				t.Errorf("a legacy row encodes %v as %s, want no key: its stored bytes must not change", path, v)
			}
		}

		raw, err = bson.Marshal(spine)
		if err != nil {
			t.Fatalf("bson.Marshal: %v", err)
		}
		for _, c := range []struct {
			path []string
			want string
		}{
			{[]string{"subscription_id"}, "sub-1"},
			{[]string{"routed_by"}, RoutingSpine},
			{[]string{"card_id"}, "card-1"},
			{[]string{"retry_target", "subscription_id"}, "sub-1"},
			{[]string{"retry_target", "routed_by"}, RoutingSpine},
		} {
			v, ok := bsonField(t, raw, c.path...)
			if !ok {
				t.Errorf("a spine row encodes no %v", c.path)
				continue
			}
			if s, isString := v.StringValueOK(); !isString || s != c.want {
				t.Errorf("%v = %s, want the string %q", c.path, v, c.want)
			}
		}
		var back Delivery
		if err := bson.Unmarshal(raw, &back); err != nil {
			t.Fatalf("bson.Unmarshal: %v", err)
		}
		if back.SubscriptionID != "sub-1" || back.RoutedBy != RoutingSpine || back.CardID != "card-1" {
			t.Fatalf("row attribution after the round trip = %q / %q / %q", back.SubscriptionID, back.RoutedBy, back.CardID)
		}
		if back.RetryTarget == nil || back.RetryTarget.SubscriptionID != "sub-1" || back.RetryTarget.RoutedBy != RoutingSpine {
			t.Fatalf("parked target after the round trip = %+v, want sub-1 / spine", back.RetryTarget)
		}
	})

	t.Run("json", func(t *testing.T) {
		keys := func(d Delivery) map[string]json.RawMessage {
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var out map[string]json.RawMessage
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			return out
		}
		public := keys(legacy)
		for _, key := range []string{"subscription_id", "routed_by", "card_id", "SubscriptionID", "RoutedBy", "CardID"} {
			if v, ok := public[key]; ok {
				t.Errorf("a legacy row answers %s:%s, want no key", key, v)
			}
		}
		public = keys(spine)
		for key, want := range map[string]string{"subscription_id": `"sub-1"`, "routed_by": `"spine"`, "card_id": `"card-1"`} {
			if got := string(public[key]); got != want {
				t.Errorf("%s = %s, want %s: the delivery audit is where a spine target is read", key, got, want)
			}
		}
	})
}
