package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookSinkPostsSlackShape(t *testing.T) {
	type received struct {
		contentType string
		body        webhookPayload
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var p webhookPayload
		_ = json.Unmarshal(raw, &p)
		got <- received{contentType: r.Header.Get("Content-Type"), body: p}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewWebhookSink(srv.URL, nil)
	if sink == nil {
		t.Fatal("NewWebhookSink returned nil for non-empty URL")
	}
	a := Alert{
		Kind: KindStall, RunID: "r1", RunName: "nice-run", NodeID: "agent",
		Reason: "no activity for 5m0s", Link: "http://localhost:4891/runs/r1",
		Timestamp: time.Now(),
	}
	sink.Notify(context.Background(), a)

	select {
	case r := <-got:
		if r.contentType != "application/json" {
			t.Errorf("Content-Type = %q", r.contentType)
		}
		if !strings.Contains(r.body.Text, "Run stalled: nice-run") {
			t.Errorf("body missing title: %q", r.body.Text)
		}
		if !strings.Contains(r.body.Text, "http://localhost:4891/runs/r1") {
			t.Errorf("body missing link: %q", r.body.Text)
		}
		if !strings.Contains(r.body.Text, "agent") {
			t.Errorf("body missing node: %q", r.body.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("webhook was not called")
	}
}

func TestNewWebhookSinkEmptyURL(t *testing.T) {
	if NewWebhookSink("", nil) != nil {
		t.Fatal("expected nil sink for empty URL")
	}
}

func TestWebhookSinkErrorDoesNotPanic(t *testing.T) {
	// Unreachable address — Notify must swallow the error silently.
	sink := NewWebhookSink("http://127.0.0.1:1/nope", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	sink.Notify(ctx, Alert{Kind: KindRunFailed, RunID: "r1"})
}

func TestWebhookTextRendersBudget(t *testing.T) {
	a := Alert{Kind: KindBudgetWarning, RunName: "r", Axis: "tokens", BudgetPct: 82}
	txt := a.WebhookText()
	if !strings.Contains(txt, "tokens at 82%") {
		t.Errorf("WebhookText missing budget line: %q", txt)
	}
}
