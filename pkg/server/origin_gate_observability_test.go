package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// gateServer builds the smallest Server that can answer the origin gate, with
// its log captured. PublicURL is set so the allowlist has a non-loopback entry
// to reason about.
func gateServer(t *testing.T, buf *bytes.Buffer) *Server {
	t.Helper()
	return &Server{
		cfg:    Config{Port: 4123, PublicURL: "https://studio.example"},
		logger: iterlog.New(iterlog.LevelWarn, buf),
	}
}

// TestOriginGateNamesWhatItRefused is the reason this file exists. The gate
// answers 403 to the caller and, before this, told the deployment nothing —
// so "no legitimate client is being refused" and "we have no way to see one"
// produced identical evidence. The assertion is on the CONTENT of the line:
// a refusal that does not name the origin cannot be acted on.
func TestOriginGateNamesWhatItRefused(t *testing.T) {
	var buf bytes.Buffer
	s := gateServer(t, &buf)

	req := httptest.NewRequest(http.MethodPost, "/api/me/api-keys", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	if s.originGateAllows(rec, req) {
		t.Fatal("gate admitted a foreign origin on a state-changing /api/ route")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	logged := buf.String()
	for _, want := range []string{"origin gate", "refused", "POST", "/api/me/api-keys", "https://evil.example"} {
		if !strings.Contains(logged, want) {
			t.Errorf("refusal log does not mention %q — an operator cannot act on it.\nlogged: %s", want, logged)
		}
	}
}

// TestOriginGateStaysQuietWhenItAdmits guards the other half. A gate that
// logged every request would drown the signal the test above depends on, and
// the same-origin SPA is the overwhelming majority of traffic.
func TestOriginGateStaysQuietWhenItAdmits(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		host   string
	}{
		{"same-origin SPA", "https://studio.example", "studio.example"},
		{"configured public URL behind a rewriting proxy", "https://studio.example", "internal-svc.cluster.local"},
		{"non-browser caller with no Origin at all", "", "studio.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := gateServer(t, &buf)

			req := httptest.NewRequest(http.MethodPost, "/api/me/api-keys", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()

			if !s.originGateAllows(rec, req) {
				t.Fatalf("gate refused a legitimate caller (origin=%q host=%q)", tc.origin, tc.host)
			}
			if buf.Len() != 0 {
				t.Errorf("gate logged on an ADMITTED request; the refusal signal would drown.\nlogged: %s", buf.String())
			}
		})
	}
}

// TestRefusalLogCannotForgeARecord: every value in the refusal line is chosen
// by whoever is being refused. Written raw into a line-oriented log, a CRLF
// appends records of the attacker's choosing — a plausible "gate: admitted"
// one, say, which would make the log lie about the very thing it exists to
// report.
//
// What is asserted is that the refusal stays ONE record with no raw CR/LF.
// Not that the attacker's text is absent: we are logging their value on
// purpose, so it is necessarily there. Turning a value into a second record
// is the part that makes the log lie.
func TestRefusalLogCannotForgeARecord(t *testing.T) {
	forged := "2026-01-01 origin gate: admitted https://evil.example"

	cases := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"crafted path", func(r *http.Request) { r.URL.Path = "/api/x\r\n" + forged }},
		// Set the header map directly: net/http's own parser rejects this on
		// a real wire, and leaning on that would test net/http, not us.
		{"crafted origin", func(r *http.Request) {
			r.Header["Origin"] = []string{"https://evil.example\r\n" + forged}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := gateServer(t, &buf)

			req := httptest.NewRequest(http.MethodPost, "/api/me/api-keys", nil)
			req.Header.Set("Origin", "https://evil.example")
			tc.mutate(req)
			rec := httptest.NewRecorder()

			if s.originGateAllows(rec, req) {
				t.Fatal("gate admitted a foreign origin")
			}

			logged := buf.String()
			if n := strings.Count(strings.TrimRight(logged, "\n"), "\n"); n != 0 {
				t.Errorf("refusal produced %d extra log record(s) — the value can forge lines:\n%s", n, logged)
			}
			if strings.Contains(logged, "\r") {
				t.Errorf("carriage return survived into the log:\n%q", logged)
			}
			// The forged text may appear — it is the value being reported.
			// What must not appear is a line that BEGINS with it.
			for _, line := range strings.Split(strings.TrimRight(logged, "\n"), "\n") {
				if strings.HasPrefix(line, forged) {
					t.Errorf("a value became a record of its own:\n%s", logged)
				}
			}
		})
	}
}

// TestRefusalLogIsBounded: neutralising control characters says nothing about
// length. Without truncation a caller puts an arbitrarily long value in every
// refusal line — the cheap way to make a log unreadable or expensive, and the
// reason the gate logging unthrottled is defensible only while each line is
// capped.
func TestRefusalLogIsBounded(t *testing.T) {
	var buf bytes.Buffer
	s := gateServer(t, &buf)

	req := httptest.NewRequest(http.MethodPost, "/api/me/api-keys", nil)
	req.Header.Set("Origin", "https://"+strings.Repeat("a", 64*1024)+".example")
	rec := httptest.NewRecorder()

	if s.originGateAllows(rec, req) {
		t.Fatal("gate admitted a foreign origin")
	}
	if got := buf.Len(); got > 4*logSafeMax {
		t.Errorf("refusal line is %d bytes for a 64KiB origin — it is not bounded", got)
	}
}

func TestLogSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain value passes through", "https://ok.example", "https://ok.example"},
		{"empty stays empty", "", ""},
		{"newline neutralised", "a\nb", "a.b"},
		{"carriage return neutralised", "a\rb", "a.b"},
		{"tab and NUL neutralised", "a\tb\x00c", "a.b.c"},
		{"DEL neutralised", "a\x7fb", "a.b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := logSafe(tc.in); got != tc.want {
				t.Errorf("logSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("long value is truncated and marked", func(t *testing.T) {
		got := logSafe(strings.Repeat("x", logSafeMax+50))
		if !strings.HasSuffix(got, "…") {
			t.Errorf("truncation is not signalled: %q", got)
		}
		if len([]rune(strings.TrimSuffix(got, "…"))) != logSafeMax {
			t.Errorf("truncated to %d runes, want %d", len([]rune(strings.TrimSuffix(got, "…"))), logSafeMax)
		}
	})
}

// TestExtraAllowedOriginsAdmitsASecondPublicHost covers the case the env var
// exists for: a deployment reachable on two hosts, where PublicURL can only
// name one.
//
// It drives the WHOLE chain — env → BrowserGuard → allowedOrigins → the gate's
// verdict on a real request — because testing splitAllowedOrigins alone would
// stay green with the wiring deleted, which is how a capability ships dead
// under a passing test.
func TestExtraAllowedOriginsAdmitsASecondPublicHost(t *testing.T) {
	t.Setenv("ITERION_ALLOWED_ORIGINS", "https://second.example, https://third.example:8443")

	reached := false
	guard := BrowserGuard(4123, "https://first.example", iterlog.New(iterlog.LevelWarn, &bytes.Buffer{}),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true }))

	cases := []struct {
		name       string
		origin     string
		wantReach  bool
		wantStatus int
	}{
		{"PublicURL host still admitted", "https://first.example", true, http.StatusOK},
		{"second host, named by the env", "https://second.example", true, http.StatusOK},
		{"third host with an explicit port", "https://third.example:8443", true, http.StatusOK},
		{"a host named by nobody is still refused", "https://evil.example", false, http.StatusForbidden},
		{"the env does not admit a sibling of a named host", "https://x.second.example", false, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodPost, "/api/v1/native/issues", nil)
			req.Host = "internal-svc.cluster.local" // force the allowlist branch, not same-origin
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()

			guard.ServeHTTP(rec, req)

			if reached != tc.wantReach {
				t.Errorf("handler reached = %v, want %v (origin %q)", reached, tc.wantReach, tc.origin)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

// TestExtraAllowedOriginsIsOffByDefault: the env var must not weaken a
// deployment that never sets it.
func TestExtraAllowedOriginsIsOffByDefault(t *testing.T) {
	t.Setenv("ITERION_ALLOWED_ORIGINS", "")

	guard := BrowserGuard(4123, "https://first.example", nil,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("handler reached: an unnamed origin was admitted with the env unset")
		}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/native/issues", nil)
	req.Host = "internal-svc.cluster.local"
	req.Header.Set("Origin", "https://second.example")
	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestTrailingSlashIsAcceptedNotScolded: "https://host/" is how a human
// writes an origin. It carries nothing ambiguous, so refusing it would spend
// an operator's afternoon on a slash.
func TestTrailingSlashIsAcceptedNotScolded(t *testing.T) {
	valid, malformed := splitAllowedOrigins("https://second.example/")
	if len(malformed) != 0 {
		t.Errorf("a trailing slash was reported malformed: %v", malformed)
	}
	if len(valid) != 1 || valid[0] != "https://second.example" {
		t.Errorf("valid = %v, want [https://second.example] — the slash must be dropped, since an Origin header never carries one", valid)
	}
}

func TestSplitAllowedOriginsNormalises(t *testing.T) {
	valid, malformed := splitAllowedOrigins("https://second.example, https://third.example:8443")
	if len(malformed) != 0 {
		t.Fatalf("well-formed origins reported malformed: %v", malformed)
	}
	want := []string{"https://second.example", "https://third.example:8443"}
	if len(valid) != len(want) {
		t.Fatalf("valid = %v, want %v", valid, want)
	}
	for i := range want {
		if valid[i] != want[i] {
			t.Errorf("valid[%d] = %q, want %q", i, valid[i], want[i])
		}
	}
}

// TestMalformedAllowedOriginIsReportedNotSwallowed: a typo'd entry is
// functionally identical to an absent one — the origin is refused either way.
// That is the shape of a guard that looks configured and matches nothing, so
// the entry has to be named rather than dropped.
func TestMalformedAllowedOriginIsReportedNotSwallowed(t *testing.T) {
	cases := []string{
		"second.example",          // no scheme
		"https://",                // no host
		"https://x.example/path",  // a path asks for scoping the gate cannot do
		"https://x.example/?a=b",  // ditto a query
		"https://x.example/#frag", // ditto a fragment
		"not a url at all %%%",    // unparseable
	}
	for _, entry := range cases {
		t.Run(entry, func(t *testing.T) {
			valid, malformed := splitAllowedOrigins(entry)
			if len(valid) != 0 {
				t.Errorf("admitted %q as origin %v", entry, valid)
			}
			if len(malformed) != 1 || malformed[0] != entry {
				t.Errorf("malformed = %v, want exactly [%q]", malformed, entry)
			}
		})
	}

	t.Run("the warning names the offending entry", func(t *testing.T) {
		t.Setenv("ITERION_ALLOWED_ORIGINS", "https://good.example,second.example")
		var buf bytes.Buffer
		valid := loadExtraAllowedOrigins(iterlog.New(iterlog.LevelWarn, &buf))

		if len(valid) != 1 || valid[0] != "https://good.example" {
			t.Errorf("valid = %v, want just the good entry", valid)
		}
		logged := buf.String()
		if !strings.Contains(logged, "second.example") {
			t.Errorf("warning does not name the bad entry:\n%s", logged)
		}
		if strings.Contains(logged, "https://good.example") {
			t.Errorf("warning names a VALID entry, which would send the operator after the wrong one:\n%s", logged)
		}
	})

	t.Run("a nil logger does not panic", func(t *testing.T) {
		t.Setenv("ITERION_ALLOWED_ORIGINS", "second.example")
		loadExtraAllowedOrigins(nil) // BrowserGuard may be given none
	})
}
