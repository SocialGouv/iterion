package forfait

import (
	"context"
	"strings"
	"testing"
	"time"
)

// FetchWindows is the seam the cloud publisher refreshes a forfait's
// readings through: every window the endpoint reports, utilization as the
// FRACTION the session telemetry uses, the reset instant when given, a
// null window skipped, and a non-200 (a setup-token credential lacks the
// scope and gets 403) an error rather than an empty answer.
func TestFetchWindows_ParsesEveryWindowAndReset(t *testing.T) {
	doer := &stubDoer{status: 200, body: `{
		"five_hour": {"utilization": 12.5, "resets_at": "2026-09-05T10:00:00.283698+00:00"},
		"seven_day": {"utilization": 0, "resets_at": "2026-09-08T21:00:00+00:00"},
		"seven_day_opus": null,
		"seven_day_sonnet": {"utilization": 3},
		"extra_usage": {"is_enabled": true}
	}`}
	got, err := FetchWindows(context.Background(), "tok-secret", doer)
	if err != nil {
		t.Fatalf("FetchWindows: %v", err)
	}
	if doer.gotTok != "Bearer tok-secret" || doer.gotAB != oauthBetaHeader {
		t.Fatalf("headers: auth=%q beta=%q", doer.gotTok, doer.gotAB)
	}
	if len(got) != 3 {
		t.Fatalf("got %d windows (%+v), want 3 (the null one skipped, the non-window key ignored)", len(got), got)
	}
	byName := map[string]WindowUsage{}
	for _, w := range got {
		byName[w.Window] = w
	}
	if w := byName["five_hour"]; w.Utilization != 0.125 || !w.ResetsAt.Equal(time.Date(2026, 9, 5, 10, 0, 0, 283698000, time.UTC)) {
		t.Fatalf("five_hour = %+v, want 0.125 resetting 2026-09-05T10:00:00.283698Z", w)
	}
	if w := byName["seven_day"]; w.Utilization != 0 || !w.ResetsAt.Equal(time.Date(2026, 9, 8, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("seven_day = %+v, want 0 resetting Monday 21:00Z — a zero utilization is a real reading", w)
	}
	if w := byName["seven_day_sonnet"]; w.Utilization != 0.03 || !w.ResetsAt.IsZero() {
		t.Fatalf("seven_day_sonnet = %+v, want 0.03 with no reset instant", w)
	}

	if _, err := FetchWindows(context.Background(), "tok", &stubDoer{status: 403, body: `{"error":"insufficient scope"}`}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a 403 must be an error naming the status, got %v", err)
	}
	if _, err := FetchWindows(context.Background(), "tok", &stubDoer{status: 200, body: `{"five_hour":{"utilization":"lots"}}`}); err == nil {
		t.Fatal("a malformed window must be an error, not a silent zero")
	}
}

func TestAccessTokenFromCredentialsJSON(t *testing.T) {
	if tok, ok := AccessTokenFromCredentialsJSON([]byte(`{"claudeAiOauth":{"accessToken":" tok-1 "}}`)); !ok || tok != "tok-1" {
		t.Fatalf("got %q/%v, want tok-1", tok, ok)
	}
	for _, bad := range []string{`{}`, `{"claudeAiOauth":{"accessToken":""}}`, `not json`} {
		if _, ok := AccessTokenFromCredentialsJSON([]byte(bad)); ok {
			t.Fatalf("%q yielded a token", bad)
		}
	}
}
