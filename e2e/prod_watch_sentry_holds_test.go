package e2e

import (
	"strings"
	"testing"
	"time"
)

// An archived-until-escalating issue that escalates posts ESCALATING (its
// archived note already said), and its closing note is owed again after.
func TestProdWatch_SentryArchivedIssueEscalatingPosts(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "93", ShortID: strp("P-93"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.edit("93", func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.Status = "ignored"
		i.Substatus = strp("archived_until_escalating")
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-93:low" {
		t.Fatalf("setup archived note: %v", got)
	}
	t1 := time.Now().Add(time.Second)
	h.sentry.edit("93", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("escalating")
		i.LastSeen = t1
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_escalating", At: t1})
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "escalating:P-93:medium" {
		t.Fatalf("an archived issue escalating: %v", got)
	}
	h.sentry.edit("93", func(i *pwSentryIssue) { i.Status = "ignored"; i.Substatus = strp("archived_until_escalating") })
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-93:low" {
		t.Fatalf("archived again after its escalation posted: the note is owed again: %v", got)
	}
}

// A pending escalation followed by a dated regression: one alert, the
// regression, at the severity the escalation reached; nothing pending after.
func TestProdWatch_SentryPendingEscalationThenRegression(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "10", ShortID: strp("P-10"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.alertCap.Store(1)
	h.sentry.edit("10", func(i *pwSentryIssue) {
		i.Level = "fatal"
		i.LastSeen = time.Now().Add(time.Second)
		i.FirstProcessed = now.Add(-3 * time.Hour)
	})
	h.sentry.put(&pwSentryIssue{ID: "09", ShortID: strp("P-09"), Title: "y", Level: "fatal", FirstProcessed: time.Now(), LastSeen: time.Now()})
	sentryTick(t, h, wf)
	if rec := sentryIncident(t, h, "10"); rec["pending"] != "escalated" {
		t.Fatalf("setup: %v", rec)
	}
	h.alertCap.Store(0)
	t1 := time.Now().Add(2 * time.Second)
	h.sentry.edit("10", func(i *pwSentryIssue) {
		i.Substatus = strp("regressed")
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1})
	})
	got := sentryAlerts(sentryTick(t, h, wf))
	if strings.Join(got, " ") != "regressed:P-10:high" {
		t.Fatalf("a pending escalation then a dated regression: want the regression alone at high, got %v", got)
	}
	if rec := sentryIncident(t, h, "10"); rec["pending"] != nil {
		t.Fatalf("still pending after the regression posted: %v", rec)
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("the next tick: %v", got)
	}
}

// A stamp set early does not spare a transition dated AFTER it (the guard
// `tr_at <= seen_tr`): a stale stamp from before a lane outage cannot turn a
// transition the lane first read days late into news.
func TestProdWatch_SentryAnEarlyStampDoesNotSpareALaterTransition(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 10*24*time.Hour)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "35", ShortID: strp("P-35"), Title: "r", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute),
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Minute)}}})
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf)
	// Stamped four days ago; the lane then went dark; the regression the
	// lookup now finds happened 30 h ago (after the stamp), first read now.
	sentryEditRecord(t, h, "35", func(rec map[string]any) {
		rec["transition_seen_at"] = now.Add(-4 * 24 * time.Hour).UTC().Format(time.RFC3339)
	})
	h.sentry.edit("35", func(i *pwSentryIssue) {
		i.Acts = []pwSentryAct{{Type: "set_regression", At: now.Add(-30 * time.Hour)}}
	})
	o := sentryTick(t, h, wf)
	if anyPrefix(sentryAlerts(o), "regressed:P-35") {
		t.Fatalf("a transition dated after a stale stamp, first read 30 h late, posted as news: %v", sentryAlerts(o))
	}
}

// A log template cut by the cap waits while the Loki lane is off and posts
// once it is back.
func TestProdWatch_PendingTemplateWaitsForItsLane(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{}}
	})
	h.lines.Store([]pwLine{{TS: nsAgo(time.Second), Line: "ERROR boot sequence failed", Container: "api", Q: "errors-q"}})
	h.tick(t, wf, false) // bootstrap
	h.alertCap.Store(1)
	h.healthStatus.Store(503) // a critical probe takes the only slot
	h.lines.Store(append(h.lines.Load().([]pwLine), pwLine{TS: nsAgo(500 * time.Millisecond), Line: "ERROR alpha handler failed", Container: "api", Q: "errors-q"}))
	got := alertsOf(t, h.tick(t, wf, false))
	fp := pwTemplateFP("ERROR alpha handler failed")
	st := h.state(t)
	rec, _ := st["incidents"].(map[string]any)[fp].(map[string]any)
	if rec == nil || rec["pending"] != "new" {
		t.Fatalf("setup: the template is not pending (%v): %v", got, rec)
	}
	h.healthStatus.Store(200)
	h.alertCap.Store(0)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{}}
		delete(cfg, "loki")
		delete(cfg, "grafana")
	})
	if got := alertsOf(t, h.tick(t, wf, false)); strings.Contains(strings.Join(got, " "), "loki:") {
		t.Fatalf("Loki lane off: the pending template was re-emitted: %v", got)
	}
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{}}
	})
	if got := alertsOf(t, h.tick(t, wf, false)); !strings.Contains(strings.Join(got, " "), "loki:new:") {
		t.Fatalf("Loki lane back: the pending template did not post: %v", got)
	}
}
