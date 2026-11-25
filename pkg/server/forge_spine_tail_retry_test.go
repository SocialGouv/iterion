package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// spineTailFixture is one issue-label event on a GitHub config, entering the
// launch tail the way the router hands it over: resolved targets, claimed and
// launched by acceptWebhookTargets. The launcher is the tail's test seam.
type spineTailFixture struct {
	s      *Server
	cfg    webhooks.Config
	secret string
	meta   webhookEventMeta
	// launch is what the launcher answers next.
	launch func() (string, error)
}

func newSpineTailFixture(t *testing.T) *spineTailFixture {
	t.Helper()
	fx := &spineTailFixture{s: newWebhookTestServer(t)}
	fx.cfg, fx.secret = issueConfig(t, fx.s)
	if err := fx.s.webhookConfigs.Create(context.Background(), fx.cfg); err != nil {
		t.Fatalf("create webhook config: %v", err)
	}
	fx.meta = webhookEventMeta{
		Kind: "issues", Action: "labeled", ProjectPath: "acme/widgets",
		SubjectID: "issue:42", DeliveryID: "gh-delivery-1",
	}
	fx.s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return fx.launch()
	}
	return fx
}

func (fx *spineTailFixture) failing() {
	fx.launch = func() (string, error) { return "", errors.New("temporary launch failure") }
}

func (fx *spineTailFixture) launching(runID string) {
	fx.launch = func() (string, error) { return runID, nil }
}

// target is the launch target of one bot, attributed to subscriptionID by
// routedBy — both empty for a target legacy routed.
func (fx *spineTailFixture) target(subscriptionID, routedBy string) forgeLaunchTarget {
	return forgeLaunchTarget{
		BotID: "feature-dev", IdemKey: "spine-tail-claim-key",
		Vars:           map[string]string{"feature_prompt": "Add a CSV export endpoint"},
		RepoURL:        "https://github.com/acme/widgets.git",
		subscriptionID: subscriptionID, routedBy: routedBy,
	}
}

// deliver runs the tail on one target, as a forge delivery of the event does.
func (fx *spineTailFixture) deliver(t *testing.T, target forgeLaunchTarget) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := ghReq(ghCtx(fx.cfg), ghLabeledIssue, prforge.EventHeaderIssues, fx.secret)
	fx.s.acceptWebhookTargets(req.Context(), rec, req, fx.cfg, fx.meta, []forgeLaunchTarget{target}, "payload-hash", "203.0.113.7")
	return rec
}

// row returns the one delivery row the event's claim key names.
func (fx *spineTailFixture) row(t *testing.T) webhooks.Delivery {
	t.Helper()
	rows, err := fx.s.webhookDeliveries.ListByWebhook(context.Background(), fx.cfg.TenantID, fx.cfg.ID, 10)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the event left %d delivery rows, want the one row its claim key names: %+v", len(rows), rows)
	}
	return rows[0]
}

// failedRow delivers target against a failing launcher and returns the
// launch_error row the sweeper will retry.
func (fx *spineTailFixture) failedRow(t *testing.T, target forgeLaunchTarget) webhooks.Delivery {
	t.Helper()
	fx.failing()
	if rec := fx.deliver(t, target); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery: code=%d body=%s", rec.Code, rec.Body.String())
	}
	d := fx.row(t)
	if d.Status != webhooks.StatusLaunchError || d.RunID != "" || d.NextAttemptAt == nil || d.RetryTarget == nil || d.Attempts != 1 {
		t.Fatalf("the failed launch left %+v, want a retryable launch_error row at attempt 1", d)
	}
	return d
}

// wantAttribution checks the two places a delivery row carries its spine
// attribution: the row, which the audit reads, and the parked target, which
// the next retry rebuilds the launch — and so the row — from.
func wantAttribution(t *testing.T, when string, d webhooks.Delivery, subscriptionID, routedBy string) {
	t.Helper()
	if d.SubscriptionID != subscriptionID || d.RoutedBy != routedBy {
		t.Errorf("%s: the row reads subscription_id=%q routed_by=%q, want %q / %q", when, d.SubscriptionID, d.RoutedBy, subscriptionID, routedBy)
	}
	if d.RetryTarget == nil {
		t.Errorf("%s: the row parks no retry target", when)
		return
	}
	if d.RetryTarget.SubscriptionID != subscriptionID || d.RetryTarget.RoutedBy != routedBy {
		t.Errorf("%s: the parked target reads subscription_id=%q routed_by=%q, want %q / %q", when, d.RetryTarget.SubscriptionID, d.RetryTarget.RoutedBy, subscriptionID, routedBy)
	}
}

// A retry rewrites the delivery row whole: the claim (ClaimFailedRetry)
// replaces the document with a row rebuilt from the parked target. The spine
// attribution — the subscription that claimed the event and the router that
// decided it — must come out of every such rewrite as it went in, or the
// audit loses it on the first transient launch failure.
func TestWebhookRetry_KeepsSpineAttribution(t *testing.T) {
	const sub = "sub-feature-dev-1"

	t.Run("the sweeper relaunches the row", func(t *testing.T) {
		fx := newSpineTailFixture(t)
		d := fx.failedRow(t, fx.target(sub, webhooks.RoutingSpine))
		wantAttribution(t, "after the failed first launch", d, sub, webhooks.RoutingSpine)

		fx.launching("run-retry")
		fx.s.sweepWebhookRetries(context.Background(), *d.NextAttemptAt)
		got := fx.row(t)
		if got.ID != d.ID || got.Status != webhooks.StatusLaunched || got.RunID != "run-retry" || got.Attempts != 2 {
			t.Fatalf("the sweep left %+v, want the same row launched as run-retry at attempt 2", got)
		}
		wantAttribution(t, "after the sweeper's relaunch", got, sub, webhooks.RoutingSpine)
	})

	t.Run("the sweeper's relaunch fails, then succeeds", func(t *testing.T) {
		fx := newSpineTailFixture(t)
		d := fx.failedRow(t, fx.target(sub, webhooks.RoutingSpine))

		fx.s.sweepWebhookRetries(context.Background(), *d.NextAttemptAt)
		again := fx.row(t)
		if again.Status != webhooks.StatusLaunchError || again.Attempts != 2 || again.NextAttemptAt == nil {
			t.Fatalf("the second failure left %+v, want a retryable launch_error row at attempt 2", again)
		}
		wantAttribution(t, "after the second failed launch", again, sub, webhooks.RoutingSpine)

		fx.launching("run-third")
		fx.s.sweepWebhookRetries(context.Background(), *again.NextAttemptAt)
		got := fx.row(t)
		if got.Status != webhooks.StatusLaunched || got.RunID != "run-third" || got.Attempts != 3 {
			t.Fatalf("the third attempt left %+v, want the row launched as run-third at attempt 3", got)
		}
		wantAttribution(t, "after the third attempt", got, sub, webhooks.RoutingSpine)
	})

	t.Run("an operator retries the row", func(t *testing.T) {
		fx := newSpineTailFixture(t)
		d := fx.failedRow(t, fx.target(sub, webhooks.RoutingSpine))

		fx.launching("run-manual")
		out, launch := fx.s.prepareWebhookRetry(context.Background(), d)
		if launch == nil {
			t.Fatalf("the manual retry claimed nothing: %+v", out)
		}
		if res := launch(context.Background()); res.Status != webhooks.StatusLaunched {
			t.Fatalf("the manual retry answered %+v, want launched", res)
		}
		got := fx.row(t)
		if got.RunID != "run-manual" || got.Attempts != 2 {
			t.Fatalf("the manual retry left %+v, want the row launched as run-manual at attempt 2", got)
		}
		wantAttribution(t, "after the manual retry", got, sub, webhooks.RoutingSpine)
	})

	// The forge redelivers the event: the router resolves the target again
	// and the tail takes the failed row over. The row is rebuilt from THAT
	// target, so it carries the subscription that claims the event now.
	t.Run("a forge redelivery takes the failed row over", func(t *testing.T) {
		fx := newSpineTailFixture(t)
		d := fx.failedRow(t, fx.target(sub, webhooks.RoutingSpine))

		const reprojected = "sub-feature-dev-2"
		fx.launching("run-redelivered")
		if rec := fx.deliver(t, fx.target(reprojected, webhooks.RoutingSpine)); rec.Code != http.StatusAccepted {
			t.Fatalf("redelivery: code=%d body=%s", rec.Code, rec.Body.String())
		}
		got := fx.row(t)
		if got.ID != d.ID || got.Status != webhooks.StatusLaunched || got.RunID != "run-redelivered" || got.Attempts != 2 {
			t.Fatalf("the redelivery left %+v, want the same row launched as run-redelivered at attempt 2", got)
		}
		wantAttribution(t, "after the redelivery's take-over", got, reprojected, webhooks.RoutingSpine)
	})

	// A retry the tail refuses before any launch still rewrites the row —
	// here a webhook disabled since the failure.
	t.Run("a refused retry closes the row", func(t *testing.T) {
		fx := newSpineTailFixture(t)
		d := fx.failedRow(t, fx.target(sub, webhooks.RoutingSpine))

		stored, err := fx.s.webhookConfigs.Get(context.Background(), fx.cfg.ID)
		if err != nil {
			t.Fatalf("get config: %v", err)
		}
		stored.Enabled = false
		if err := fx.s.webhookConfigs.Update(context.Background(), stored); err != nil {
			t.Fatalf("disable config: %v", err)
		}
		fx.launch = func() (string, error) {
			t.Error("a disabled webhook launched its retry")
			return "", errors.New("unreachable")
		}
		fx.s.sweepWebhookRetries(context.Background(), *d.NextAttemptAt)
		got := fx.row(t)
		if got.Status != webhooks.StatusLaunchExhausted || got.Error != "webhook_retry_disabled" {
			t.Fatalf("the refused retry left %+v, want launch_exhausted / webhook_retry_disabled", got)
		}
		wantAttribution(t, "after the refused retry", got, sub, webhooks.RoutingSpine)
	})

	// The control: a target legacy routed stays unattributed through the same
	// rewrites, so the assertions above read the target and not a constant.
	t.Run("a legacy target stays unattributed", func(t *testing.T) {
		fx := newSpineTailFixture(t)
		d := fx.failedRow(t, fx.target("", ""))
		wantAttribution(t, "after the failed first launch", d, "", "")

		fx.launching("run-legacy")
		fx.s.sweepWebhookRetries(context.Background(), *d.NextAttemptAt)
		got := fx.row(t)
		if got.Status != webhooks.StatusLaunched || got.RunID != "run-legacy" {
			t.Fatalf("the sweep left %+v, want the row launched as run-legacy", got)
		}
		wantAttribution(t, "after the sweeper's relaunch", got, "", "")
	})
}
