package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Everything the sentinels do not cover used to become a bare
// fmt.Errorf("…: HTTP %d"), so a caller could only re-parse the message —
// and every handler that could not classify it answered 500. The status is
// the fact; it is typed.
func TestStatusErrTypesTheUpstreamStatus(t *testing.T) {
	cases := []struct {
		code     int
		wantType bool
		wantIs   error
	}{
		{http.StatusUnauthorized, false, ErrUnauthorized},
		{http.StatusForbidden, false, ErrForbidden},
		{http.StatusNotFound, false, ErrNotFound},
		{http.StatusTooManyRequests, true, nil},
		{http.StatusServiceUnavailable, true, nil},
		{http.StatusInternalServerError, true, nil},
		{http.StatusConflict, true, nil},
	}
	for _, tc := range cases {
		err := StatusErr("gitlab", "create oauth app", tc.code)
		var se *StatusError
		if got := errors.As(err, &se); got != tc.wantType {
			t.Errorf("HTTP %d: typed as *StatusError = %v, want %v (err=%v)", tc.code, got, tc.wantType, err)
			continue
		}
		if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
			t.Errorf("HTTP %d: err = %v, want errors.Is %v", tc.code, err, tc.wantIs)
		}
		if !tc.wantType {
			continue
		}
		if se.Code != tc.code {
			t.Errorf("HTTP %d: Code = %d", tc.code, se.Code)
		}
		if se.Op != "create oauth app" || se.Provider != "gitlab" {
			t.Errorf("HTTP %d: %+v, want the operation and provider named", tc.code, se)
		}
		// The message is the one every log and every operator already reads.
		if want := "gitlab: create oauth app: HTTP " + itoa(tc.code); se.Error() != want {
			t.Errorf("HTTP %d: message = %q, want %q", tc.code, se.Error(), want)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A rate limiter says how long to wait, in a header only the response
// carries. Dropped at the call site, the caller can only guess.
func TestDoTypedCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	h := NewAdminHTTP(srv.Client(), srv.URL, "gitlab", nil)
	err := h.DoTyped(context.Background(), http.MethodPost, "/applications", "create oauth app", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if se.Code != http.StatusTooManyRequests {
		t.Errorf("Code = %d, want 429", se.Code)
	}
	if se.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %v, want 42s — the forge said how long to wait", se.RetryAfter)
	}
	if !se.RateLimited() {
		t.Error("RateLimited() = false on a 429")
	}
}

func TestDoTypedIsSilentOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	defer srv.Close()
	var out struct {
		ID int `json:"id"`
	}
	h := NewAdminHTTP(srv.Client(), srv.URL, "gitlab", nil)
	if err := h.DoTyped(context.Background(), http.MethodGet, "/x", "get x", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != 7 {
		t.Fatalf("out = %+v, want the 2xx body decoded", out)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"30", 30 * time.Second},
		{"-5", 0},           // a past delta is no delay
		{"not a number", 0}, // never a panic, never a guess
	}
	for _, tc := range cases {
		if got := ParseRetryAfter(tc.in); got != tc.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// The HTTP-date form, which GitHub uses on a secondary rate limit.
	future := time.Now().UTC().Add(90 * time.Second).Format(http.TimeFormat)
	if got := ParseRetryAfter(future); got < 60*time.Second || got > 95*time.Second {
		t.Errorf("ParseRetryAfter(<+90s date>) = %v, want ~90s", got)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)
	if got := ParseRetryAfter(past); got != 0 {
		t.Errorf("ParseRetryAfter(<past date>) = %v, want 0", got)
	}
}
