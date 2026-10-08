package forge

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The label rides every request the installation client sends — a redirect
// hop included — and only that client's: the client it wraps keeps sending
// unlabelled, and an id of 0 labels nothing.
func TestInstallationClient_LabelsEveryRequestItSends(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			http.Redirect(w, r, "/b", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	var (
		mu   sync.Mutex
		sent []string
	)
	base := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		id, ok := InstallationOf(r.Context())
		mu.Lock()
		sent = append(sent, fmt.Sprintf("%s %d %v", r.URL.Path, id, ok))
		mu.Unlock()
		return http.DefaultTransport.RoundTrip(r)
	})}
	for _, c := range []*http.Client{InstallationClient(base, 42), base, InstallationClient(base, 0)} {
		resp, err := c.Get(srv.URL + "/a")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	want := "/a 42 true, /b 42 true, /a 0 false, /b 0 false, /a 0 false, /b 0 false"
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(sent, ", "); got != want {
		t.Errorf("requests sent = %q, want %q", got, want)
	}
	if InstallationClient(base, 0) != base {
		t.Error("an id of 0 names no installation: the client must come back as it is")
	}
}

// The budget GitHub reports on every answer: what is left, out of how much,
// and when the window resets. No remaining count, no budget; a limit or a
// reset it did not report stays zero rather than guessed.
func TestRateLimitBudgetOf(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	reset := now.Add(20 * time.Minute)
	epoch := strconv.FormatInt(reset.Unix(), 10)
	cases := []struct {
		name string
		hdr  map[string]string
		want RateLimitBudget
		ok   bool
	}{
		{"every header", map[string]string{"X-RateLimit-Remaining": "4210", "X-RateLimit-Limit": "5000", "X-RateLimit-Reset": epoch}, RateLimitBudget{Remaining: 4210, Limit: 5000, ResetAt: reset}, true},
		{"spent to zero", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Limit": "5000", "X-RateLimit-Reset": epoch}, RateLimitBudget{Remaining: 0, Limit: 5000, ResetAt: reset}, true},
		{"no limit or reset said", map[string]string{"X-RateLimit-Remaining": "17"}, RateLimitBudget{Remaining: 17}, true},
		{"an unreadable limit and reset", map[string]string{"X-RateLimit-Remaining": "17", "X-RateLimit-Limit": "lots", "X-RateLimit-Reset": "later"}, RateLimitBudget{Remaining: 17}, true},
		{"no remaining count", map[string]string{"X-RateLimit-Limit": "5000", "X-RateLimit-Reset": epoch}, RateLimitBudget{}, false},
		{"an unreadable remaining count", map[string]string{"X-RateLimit-Remaining": "some"}, RateLimitBudget{}, false},
		{"a negative remaining count", map[string]string{"X-RateLimit-Remaining": "-1"}, RateLimitBudget{}, false},
	}
	for _, c := range cases {
		hdr := http.Header{}
		for k, v := range c.hdr {
			hdr.Set(k, v)
		}
		got, ok := RateLimitBudgetOf(hdr, now)
		if ok != c.ok || got.Remaining != c.want.Remaining || got.Limit != c.want.Limit || !got.ResetAt.Equal(c.want.ResetAt) {
			t.Errorf("%s: RateLimitBudgetOf = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
