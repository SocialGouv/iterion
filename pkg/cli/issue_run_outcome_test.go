package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// A board card carries the outcome of the run it launched (#1501:
// last_run_status / last_run_error / last_run_error_code / last_run_at,
// stamped by the run-outcome spine). The CLI surfaces are where a
// self-hosted operator reads a card, so a card whose run DIED must not print
// the same thing as a card that was dispatched and worked: the detail view
// names the result and the message, the cloud list carries the status in its
// own column.
func TestIssueShowPrintsTheCardsRunOutcome(t *testing.T) {
	storeDir := t.TempDir()
	opts := IssueCommonOptions{StoreDir: storeDir}

	var created bytes.Buffer
	if err := RunIssueCreate(&Printer{W: &created, Format: OutputHuman}, IssueCreateOptions{
		IssueCommonOptions: opts,
		Title:              "the card whose run died",
		State:              "ready",
	}); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	s, _, err := openNativeStore(opts)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cards, err := s.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("board holds %d cards, want 1", len(cards))
	}
	id := cards[0].ID
	// The launcher writes the pointer; the spine writes the result. Both
	// halves are needed: SetRunOutcome is gated on the card still naming
	// the run it stamps.
	if err := s.SetLastRun(id, "run-dead", ""); err != nil {
		t.Fatalf("set last run: %v", err)
	}
	at := time.Date(2026, 10, 28, 21, 0, 0, 0, time.UTC)
	if err := s.SetRunOutcome(id, "run-dead", "failed", "sandbox start: no runtime", "SANDBOX_DRIVER_UNAVAILABLE", at); err != nil {
		t.Fatalf("set run outcome: %v", err)
	}

	var out bytes.Buffer
	if err := RunIssueShow(&Printer{W: &out, Format: OutputHuman}, IssueRefOptions{
		IssueCommonOptions: opts,
		IDOrPrefix:         id,
	}); err != nil {
		t.Fatalf("show issue: %v", err)
	}
	got := out.String()
	t.Logf("OBSERVED issue show:\n%s", got)

	for _, want := range []string{
		"Last run:",
		"run-dead",
		"Last run result:",
		"failed · SANDBOX_DRIVER_UNAVAILABLE · 2026-10-28 21:00:00 UTC",
		"Last run error:",
		"sandbox start: no runtime",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("issue show output does not contain %q — the card reports the dispatch and hides the result:\n%s", want, got)
		}
	}
}

// A card with no terminal outcome yet prints no result line at all: a dash or
// an empty "Last run result:" would read like a verdict the spine has not
// reached.
func TestIssueShowSaysNothingBeforeTheFirstOutcome(t *testing.T) {
	storeDir := t.TempDir()
	opts := IssueCommonOptions{StoreDir: storeDir}
	var created bytes.Buffer
	if err := RunIssueCreate(&Printer{W: &created, Format: OutputHuman}, IssueCreateOptions{
		IssueCommonOptions: opts,
		Title:              "freshly dispatched",
		State:              "ready",
	}); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	s, _, err := openNativeStore(opts)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cards, err := s.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	id := cards[0].ID
	if err := s.SetLastRun(id, "run-live", ""); err != nil {
		t.Fatalf("set last run: %v", err)
	}

	var out bytes.Buffer
	if err := RunIssueShow(&Printer{W: &out, Format: OutputHuman}, IssueRefOptions{
		IssueCommonOptions: opts,
		IDOrPrefix:         id,
	}); err != nil {
		t.Fatalf("show issue: %v", err)
	}
	got := out.String()
	t.Logf("OBSERVED issue show (no outcome):\n%s", got)
	if !strings.Contains(got, "run-live") {
		t.Errorf("the dispatch pointer is missing:\n%s", got)
	}
	if strings.Contains(got, "Last run result:") {
		t.Errorf("a card with no terminal outcome printed a result line:\n%s", got)
	}
}

// The cloud board list is the other CLI reader of the same record. It carries
// the status (and its classifier) in a column of its own — without it, the
// STATE column of a card whose run failed still reads "in_progress" and the
// operator has to open every card to find the one that died.
func TestRemoteIssuesListCarriesTheLastRunColumn(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/native/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[
  {"id":"i-1","title":"the one that died","state":"in_progress","bot":"feature-dev",
   "last_run_id":"run-dead","last_run_status":"failed","last_run_error_code":"SANDBOX_DRIVER_UNAVAILABLE",
   "last_run_error":"sandbox start: no runtime","last_run_at":"2026-10-28T21:00:00Z"},
  {"id":"i-2","title":"never dispatched","state":"ready"}
]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &RemoteClient{cfg: RemoteConfig{BaseURL: srv.URL, Token: "tok"}, http: srv.Client()}

	var out bytes.Buffer
	if err := RemoteIssuesList(context.Background(), c, &Printer{W: &out, Format: OutputHuman}, RemoteIssuesListOptions{}); err != nil {
		t.Fatalf("remote issues list: %v", err)
	}
	got := out.String()
	t.Logf("OBSERVED remote issues list:\n%s", got)
	if !strings.Contains(got, "LAST RUN") {
		t.Errorf("the list has no LAST RUN column:\n%s", got)
	}
	if !strings.Contains(got, "failed · SANDBOX_DRIVER_UNAVAILABLE") {
		t.Errorf("the failed card's outcome is not rendered:\n%s", got)
	}
	// The never-dispatched card must stay silent in that column.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "never dispatched") && strings.Contains(line, "failed") {
			t.Errorf("a card with no run shows an outcome: %q", line)
		}
	}
}

// FormatRunOutcome is the one formatter every CLI surface of the class uses.
// The status alone is enough to render; the classifier and the instant join
// it when the record holds them, and nothing is rendered without a status.
func TestFormatRunOutcome(t *testing.T) {
	at := time.Date(2026, 10, 28, 21, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status string
		code   string
		at     *time.Time
		want   string
	}{
		{name: "no outcome", want: ""},
		{name: "code without status is not an outcome", code: "BUDGET_EXCEEDED", want: ""},
		{name: "status alone", status: "finished", want: "finished"},
		{name: "cancelled carries no code", status: "cancelled", at: &at, want: "cancelled · 2026-10-28 21:00:00 UTC"},
		{name: "full tuple", status: "failed", code: "BUDGET_EXCEEDED", at: &at, want: "failed · BUDGET_EXCEEDED · 2026-10-28 21:00:00 UTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatRunOutcome(tc.status, tc.code, tc.at); got != tc.want {
				t.Errorf("FormatRunOutcome(%q, %q, %v) = %q, want %q", tc.status, tc.code, tc.at, got, tc.want)
			}
		})
	}
}
