package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every shape a forge uses to say "wait", and the 403s that are NOT a limit:
// a missing grant must never be read as a wait, nor a wait as a missing grant.
func TestRateLimitReadsEveryShape(t *testing.T) {
	epoch := func(d time.Duration) string { return strconv.FormatInt(time.Now().Add(d).Unix(), 10) }
	cases := []struct {
		name        string
		code        int
		hdr         map[string]string
		body        string
		wantLimited bool
		wantMin     time.Duration
		wantMax     time.Duration
	}{
		{"429 alone", http.StatusTooManyRequests, nil, "", true, 0, 0},
		{"429 with Retry-After", http.StatusTooManyRequests, map[string]string{"Retry-After": "42"}, "", true, 42 * time.Second, 42 * time.Second},
		{"429 with a delta RateLimit-Reset", http.StatusTooManyRequests, map[string]string{"RateLimit-Reset": "30"}, "", true, 30 * time.Second, 30 * time.Second},
		{"429 with an epoch RateLimit-Reset (GitLab)", http.StatusTooManyRequests, map[string]string{"RateLimit-Reset": epoch(5 * time.Minute)}, "", true, 4 * time.Minute, 5 * time.Minute},
		{"GitHub primary limit: 403, remaining 0, reset", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(10 * time.Minute)}, "", true, 9 * time.Minute, 10 * time.Minute},
		{"GitHub secondary limit: 403 with Retry-After", http.StatusForbidden, map[string]string{"Retry-After": "60"}, "", true, time.Minute, time.Minute},
		{"a 403 whose message names the limit", http.StatusForbidden, nil, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes."}`, true, 0, 0},
		{"a missing grant", http.StatusForbidden, nil, `{"message":"Resource not accessible by integration"}`, false, 0, 0},
		{"a 403 with budget left", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "12"}, `{"message":"Resource not accessible by integration"}`, false, 0, 0},
		{"a 404", http.StatusNotFound, map[string]string{"X-RateLimit-Remaining": "0"}, "", false, 0, 0},
		{"a secondary limit with budget left: the primary reset is not the wait", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "4321", "X-RateLimit-Reset": epoch(50 * time.Minute)},
			`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, true, 0, 0},
		{"a 429 with budget left: the reset is not the wait", http.StatusTooManyRequests,
			map[string]string{"X-RateLimit-Remaining": "10", "X-RateLimit-Reset": epoch(50 * time.Minute)}, "", true, 0, 0},
		{"the later of Retry-After and a spent budget's reset binds", http.StatusTooManyRequests,
			map[string]string{"Retry-After": "30", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(50 * time.Minute)}, "", true, 49 * time.Minute, 50 * time.Minute},
		{"a Retry-After later than a spent budget's reset binds", http.StatusForbidden,
			map[string]string{"Retry-After": "3600", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(10 * time.Minute)}, "", true, time.Hour, time.Hour},
		{"a spent budget whose reset already passed names no wait", http.StatusTooManyRequests,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(-time.Minute)}, "", true, 0, 0},
		{"a reset in epoch milliseconds is not believed", http.StatusTooManyRequests,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(time.Now().Add(10*time.Minute).UnixMilli(), 10)}, "", true, 0, 0},
		{"a Retry-After of centuries is not believed", http.StatusTooManyRequests,
			map[string]string{"Retry-After": "27670116110"}, "", true, 0, 0},
		{"a spent budget whose JSON body carries no message", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(10 * time.Minute)},
			`{"documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`, true, 9 * time.Minute, 10 * time.Minute},
		{"GitLab: RateLimit-Remaining 0 and a delta reset", http.StatusTooManyRequests,
			map[string]string{"RateLimit-Remaining": "0", "RateLimit-Reset": "60"}, "", true, time.Minute, time.Minute},
		{"the message is read whatever its case", http.StatusForbidden, nil, `{"message":"API Rate Limit Exceeded for user ID 1."}`, true, 0, 0},
		{"the call that spends the last unit and is refused a grant", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(30 * time.Minute)},
			`{"message":"Resource not accessible by integration"}`, false, 0, 0},
		{"a spent budget with no message to read (a proxy's page)", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(10 * time.Minute)}, "<html>Forbidden</html>", true, 9 * time.Minute, 10 * time.Minute},
	}
	for _, tc := range cases {
		hdr := http.Header{}
		for k, v := range tc.hdr {
			hdr.Set(k, v)
		}
		limited, reset := RateLimit(tc.code, hdr, []byte(tc.body))
		if limited != tc.wantLimited {
			t.Errorf("%s: limited = %v, want %v", tc.name, limited, tc.wantLimited)
			continue
		}
		if tc.wantMax == 0 {
			if !reset.IsZero() {
				t.Errorf("%s: reset = %v, want none — the forge named no wait", tc.name, reset)
			}
			continue
		}
		// The instant was fixed when the answer was read; allow for the time
		// the test itself took since.
		if wait := time.Until(reset); wait < tc.wantMin-2*time.Second || wait > tc.wantMax {
			t.Errorf("%s: wait = %v, want within [%v, %v]", tc.name, wait, tc.wantMin, tc.wantMax)
		}
	}
}

// The transport is the one point every forge client crosses, and the only
// one holding the answer's headers: a rate-limited answer leaves it as a typed
// error, so no caller's status mapping reads GitHub's 403 as a missing grant.
func TestDoJSONTypesARateLimitedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(20*time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for installation ID 7."}`))
	}))
	defer srv.Close()

	code, err := DoJSON(context.Background(), srv.Client(), http.MethodGet, srv.URL+"/repos/o/r/pulls/12?per_page=5", "github", nil, nil, nil)
	if code != http.StatusForbidden {
		t.Errorf("code = %d, want the answer's 403", code)
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v — a rate limit typed as a missing grant", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || !se.RateLimited() {
		t.Fatalf("err = %v, want a rate-limited *StatusError", err)
	}
	if se.Op != "GET /repos/o/r/pulls/12" {
		t.Errorf("Op = %q, want the method and the path, without the query", se.Op)
	}
	if wait := time.Until(se.ResetAt); wait < 19*time.Minute || wait > 20*time.Minute {
		t.Errorf("ResetAt in %v, want ~20m from X-RateLimit-Reset", wait)
	}
	msg := err.Error()
	for _, want := range []string{"github: GET /repos/o/r/pulls/12: HTTP 403: rate limited until " + se.ResetAt.UTC().Format(time.RFC3339), "(API rate limit exceeded for installation ID 7.)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to contain %q", msg, want)
		}
	}
}

// A rate-limited answer is named by the path as it was SENT: GitLab's
// "group%2Fsub%2Frepo" is one segment, and decoding it names a call nobody
// made. The query stays out of the name.
func TestARateLimitNamesThePathAsSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := DoJSON(context.Background(), srv.Client(), http.MethodGet, srv.URL+"/api/v4/projects/group%2Fsub%2Frepo/issues/7?private_token=x", "gitlab", nil, nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Op != "GET /api/v4/projects/group%2Fsub%2Frepo/issues/7" {
		t.Fatalf("err = %v, want Op naming the escaped path without the query", err)
	}
}

// A plain 403 is still the caller's to map: DoJSON hands the status back and
// StatusErr keeps it ErrForbidden.
func TestDoJSONLeavesAPlainForbiddenToTheCaller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()

	code, err := DoJSON(context.Background(), srv.Client(), http.MethodGet, srv.URL+"/x", "github", nil, nil, nil)
	if err != nil {
		t.Fatalf("err = %v, want the status handed back", err)
	}
	if code != http.StatusForbidden || !errors.Is(StatusErr("github", "GET x", code), ErrForbidden) {
		t.Fatalf("code = %d, want a 403 the caller maps to ErrForbidden", code)
	}
}

// The multipart upload shares the transport's contract.
func TestDoMultipartFileTypesARateLimitedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, _, err := DoMultipartFile(context.Background(), srv.Client(), http.MethodPut, srv.URL+"/projects/1/avatar", "gitlab", nil, "avatar", "a.png", "image/png", []byte("png"), nil)
	var se *StatusError
	if !errors.As(err, &se) || !se.RateLimited() {
		t.Fatalf("err = %v, want a rate-limited *StatusError", err)
	}
	if wait := time.Until(se.ResetAt); wait < 88*time.Second || wait > 90*time.Second {
		t.Errorf("ResetAt in %v, want ~90s from Retry-After", wait)
	}
}

// The instant is rendered in UTC wherever the process runs: a stored skip
// reason must read the same on every replica.
func TestStatusErrorRendersTheResetInUTC(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	err := &StatusError{Provider: "github", Op: "GET pull", Code: http.StatusForbidden, Limit: true,
		ResetAt: time.Date(2026, 9, 30, 18, 36, 8, 0, cest)}
	if got, want := err.Error(), "github: GET pull: HTTP 403: rate limited until 2026-09-30T16:36:08Z"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// A 429 that said nothing keeps the message every operator already reads,
// built by the producer that makes it (the transport, through DoTyped) — the
// transport marks every limited answer, a 429 included.
func TestStatusErrorMessageKeepsTheBareForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := NewAdminHTTP(srv.Client(), srv.URL, "gitlab", nil).DoTyped(context.Background(), http.MethodPost, "/applications", "create oauth app", nil, nil)
	if err == nil {
		t.Fatal("err = nil on a 429")
	}
	if got, want := err.Error(), "gitlab: create oauth app: HTTP 429"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}
