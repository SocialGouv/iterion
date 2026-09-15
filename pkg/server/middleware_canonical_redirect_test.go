package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func canonicalTestHandler(t *testing.T, enabled bool, publicURL string) (http.Handler, *bool) {
	t.Helper()
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	return canonicalRedirect(enabled, publicURL, next), &reached
}

func TestCanonicalRedirectMovesDocumentNavigations(t *testing.T) {
	h, reached := canonicalTestHandler(t, true, "https://iterion.cloud")

	req := httptest.NewRequest("GET", "http://iterion.fabrique.social.gouv.fr/orgs/5f916212?tab=audit", nil)
	req.Host = "iterion.fabrique.social.gouv.fr"
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://iterion.cloud/orgs/5f916212?tab=audit" {
		t.Fatalf("location: got %q", got)
	}
	if *reached {
		t.Fatal("redirected request still reached the handler")
	}
}

// Everything this middleware must leave alone. Each case names what breaks if
// it is redirected, because a redirect here is invisible until the thing on the
// other end fails.
func TestCanonicalRedirectLeavesNonDocumentsAlone(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
		why     string
	}{
		{
			name:    "inbound webhook",
			method:  "POST",
			target:  "/api/forge/webhook",
			headers: map[string]string{"Content-Type": "application/json"},
			why:     "the forge records a non-2xx delivery and stops sending",
		},
		{
			name:    "API GET from a browser page",
			method:  "GET",
			target:  "/api/runs",
			headers: map[string]string{"Sec-Fetch-Dest": "empty", "Accept": "application/json"},
			why:     "the SDK and CLI address whichever host the operator configured",
		},
		{
			name:    "API document navigation (run artifact)",
			method:  "GET",
			target:  "/api/runs/abc/artifacts/report.html",
			headers: map[string]string{"Sec-Fetch-Dest": "document"},
			why:     "/api/ answers on every host, artifacts included",
		},
		{
			name:    "liveness probe",
			method:  "GET",
			target:  "/healthz",
			headers: map[string]string{"Accept": "*/*"},
			why:     "a probe redirected off its own pod fails the pod",
		},
		{
			name:    "module preload",
			method:  "GET",
			target:  "/assets/index-BPVililS.js",
			headers: map[string]string{"Sec-Fetch-Dest": "script"},
			why:     "a cross-origin hop for a chunk costs a round trip and a CORS decision",
		},
		{
			// The witness for the method guard. Without a non-/api POST here,
			// the guard is never exercised — every other POST case is stopped
			// one line earlier by the /api/ prefix, so deleting the method
			// check leaves the suite green.
			name:    "non-API document POST",
			method:  "POST",
			target:  "/",
			headers: map[string]string{"Sec-Fetch-Dest": "document"},
			why:     "a browser re-issues a 302'd POST as a GET and drops the body",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, reached := canonicalTestHandler(t, true, "https://iterion.cloud")
			req := httptest.NewRequest(tc.method, "http://other.example"+tc.target, nil)
			req.Host = "other.example"
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusFound {
				t.Fatalf("redirected %s — %s", tc.name, tc.why)
			}
			if !*reached {
				t.Fatalf("%s never reached the handler", tc.name)
			}
		})
	}
}

func TestCanonicalRedirectPassesThroughWhenNotApplicable(t *testing.T) {
	cases := []struct {
		name      string
		enabled   bool
		publicURL string
		host      string
	}{
		{"disabled", false, "https://iterion.cloud", "iterion.fabrique.social.gouv.fr"},
		{"no public url", true, "", "iterion.fabrique.social.gouv.fr"},
		{"unparseable public url", true, "://nonsense", "iterion.fabrique.social.gouv.fr"},
		{"already canonical", true, "https://iterion.cloud", "iterion.cloud"},
		{"already canonical, other case", true, "https://iterion.cloud", "ITERION.CLOUD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, reached := canonicalTestHandler(t, tc.enabled, tc.publicURL)
			req := httptest.NewRequest("GET", "http://"+tc.host+"/orgs/x", nil)
			req.Host = tc.host
			req.Header.Set("Sec-Fetch-Dest", "document")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusFound {
				t.Fatalf("%s: redirected when it should not have", tc.name)
			}
			if !*reached {
				t.Fatalf("%s: never reached the handler", tc.name)
			}
		})
	}
}

// A browser serialises `https://host:443` as Host `host`, so comparing the
// configured host verbatim means the target it was just sent to never matches
// — every navigation loops forever and the whole browser surface is down.
func TestCanonicalRedirectConvergesOnDefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		publicURL string
		host      string
	}{
		{"https://iterion.cloud:443", "iterion.cloud"},
		{"http://iterion.cloud:80", "iterion.cloud"},
		{"https://ITERION.cloud", "iterion.cloud"},
		{"https://iterion.cloud/", "iterion.cloud"},
	} {
		t.Run(tc.publicURL, func(t *testing.T) {
			h, reached := canonicalTestHandler(t, true, tc.publicURL)
			req := httptest.NewRequest("GET", "http://"+tc.host+"/orgs/x", nil)
			req.Host = tc.host
			req.Header.Set("Sec-Fetch-Dest", "document")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusFound {
				t.Fatalf("redirected a request already ON the canonical origin → loop (Location %q)", rec.Header().Get("Location"))
			}
			if !*reached {
				t.Fatal("never reached the handler")
			}
		})
	}
}

// A PublicURL carrying a path, query, fragment or userinfo must not weld any of
// it in front of the request path: the deep link the user asked for is lost and
// the Location stops naming the origin that was configured.
func TestCanonicalRedirectBuildsLocationFromTheOriginOnly(t *testing.T) {
	for _, publicURL := range []string{
		"https://iterion.cloud",
		"https://iterion.cloud/",
		"https://iterion.cloud:443",
		"https://iterion.cloud#frag",
		"https://iterion.cloud?a=b",
		"https://user:pass@iterion.cloud",
	} {
		t.Run(publicURL, func(t *testing.T) {
			h, _ := canonicalTestHandler(t, true, publicURL)
			req := httptest.NewRequest("GET", "http://other.example/orgs/5f916212?tab=audit", nil)
			req.Host = "other.example"
			req.Header.Set("Sec-Fetch-Dest", "document")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			const want = "https://iterion.cloud/orgs/5f916212?tab=audit"
			if got := rec.Header().Get("Location"); got != want {
				t.Fatalf("Location = %q, want %q", got, want)
			}
		})
	}
}

// RequestURI() returns an opaque request target verbatim, and it does not start
// with "/". Concatenating it onto the origin puts an attacker-chosen suffix on
// the hostname: `GET foo:.evil.example/` would yield iterion.cloud.evil.example.
func TestCanonicalRedirectRefusesOpaqueTargets(t *testing.T) {
	for _, target := range []string{"foo:.evil.example/", "x:.attacker.tld/pwn?a=b"} {
		t.Run(target, func(t *testing.T) {
			h, reached := canonicalTestHandler(t, true, "https://iterion.cloud")
			req := httptest.NewRequest("GET", "/placeholder", nil)
			req.URL = &url.URL{Opaque: target}
			req.Host = "other.example"
			req.Header.Set("Sec-Fetch-Dest", "document")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if loc := rec.Header().Get("Location"); loc != "" {
				t.Fatalf("redirected an opaque target to %q", loc)
			}
			if !*reached {
				t.Fatal("never reached the handler")
			}
		})
	}
}

// The redirect decision reads request headers, so a shared cache in front of
// the non-canonical host must key on them or it serves a cached 302 to a fetch.
func TestCanonicalRedirectDeclaresItsVary(t *testing.T) {
	h, _ := canonicalTestHandler(t, true, "https://iterion.cloud")
	req := httptest.NewRequest("GET", "http://other.example/orgs/x", nil)
	req.Host = "other.example"
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Sec-Fetch-Dest") {
		t.Fatalf("Vary = %q, want it to name Sec-Fetch-Dest", got)
	}
}

// A browser that sends no Sec-Fetch-Dest still gets redirected on a navigation,
// and only on a navigation — the Accept fallback must distinguish the two.
func TestCanonicalRedirectAcceptFallback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		accept       string
		wantRedirect bool
	}{
		{"navigation accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true},
		{"wildcard accept", "*/*", false},
		{"json accept", "application/json", false},
		{"no accept at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := canonicalTestHandler(t, true, "https://iterion.cloud")
			req := httptest.NewRequest("GET", "http://other.example/orgs/x", nil)
			req.Host = "other.example"
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Code == http.StatusFound
			if got != tc.wantRedirect {
				t.Fatalf("redirect=%v want %v (status %d)", got, tc.wantRedirect, rec.Code)
			}
		})
	}
}

// requestHost reads X-Forwarded-Host, so the response depends on it and Vary
// has to name it: a cache that keys without it can store a 302 produced by a
// spoofed value and replay it to requests already on the canonical origin.
func TestCanonicalRedirectVaryNamesEveryHeaderItReads(t *testing.T) {
	h, _ := canonicalTestHandler(t, true, "https://iterion.cloud")
	req := httptest.NewRequest("GET", "http://other.example/orgs/x", nil)
	req.Host = "other.example"
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	for _, want := range []string{"Sec-Fetch-Dest", "Accept", "X-Forwarded-Host"} {
		if !strings.Contains(vary, want) {
			t.Errorf("Vary = %q, missing %q", vary, want)
		}
	}
}

// On the canonical host the navigation signals are never read — the host
// comparison decides alone — so naming them would make a CDN store each
// immutable asset once per Sec-Fetch-Dest value. The forwarded host IS read on
// every request, so it stays named.
func TestCanonicalRedirectVaryIsNarrowOnTheCanonicalHost(t *testing.T) {
	h, _ := canonicalTestHandler(t, true, "https://iterion.cloud")
	req := httptest.NewRequest("GET", "http://iterion.cloud/assets/app-Hash.js", nil)
	req.Host = "iterion.cloud"
	req.Header.Set("Sec-Fetch-Dest", "script")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	if !strings.Contains(vary, "X-Forwarded-Host") {
		t.Fatalf("Vary = %q, want it to name X-Forwarded-Host", vary)
	}
	if strings.Contains(vary, "Sec-Fetch-Dest") || strings.Contains(vary, "Accept") {
		t.Fatalf("Vary = %q names a header this branch never reads — a CDN would split every asset on it", vary)
	}
}

// A proxy that rewrites Host to an internal name leaves the original in
// X-Forwarded-Host. Comparing the rewritten value would redirect a request
// already on the canonical origin — straight into a loop.
func TestCanonicalRedirectHonoursForwardedHost(t *testing.T) {
	for _, tc := range []struct {
		name         string
		host         string
		forwarded    string
		wantRedirect bool
	}{
		{"proxy rewrote Host, client was on canonical", "iterion.svc.cluster.local", "iterion.cloud", false},
		{"proxy rewrote Host, client was elsewhere", "iterion.svc.cluster.local", "iterion.fabrique.social.gouv.fr", true},
		{"chain, client entry first", "iterion.svc.cluster.local", "iterion.cloud, inner.proxy", false},
		{"no forwarded header falls back to Host", "iterion.cloud", "", false},
		// The dangerous direction: already ON the canonical host. Taking the
		// forwarded value alone would 302 this request to the identical URL —
		// an unbounded loop as soon as the chain sets the header consistently.
		// A disagreement may only SKIP a redirect, never create one.
		{"on canonical, forwarded disagrees (spoofed)", "iterion.cloud", "evil.example", false},
		{"on canonical, forwarded is an internal name", "iterion.cloud", "iterion.svc.cluster.local", false},
		{"on canonical, forwarded is empty-ish", "iterion.cloud", " , inner.proxy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := canonicalTestHandler(t, true, "https://iterion.cloud")
			req := httptest.NewRequest("GET", "http://"+tc.host+"/orgs/x", nil)
			req.Host = tc.host
			req.Header.Set("Sec-Fetch-Dest", "document")
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-Host", tc.forwarded)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got := rec.Code == http.StatusFound; got != tc.wantRedirect {
				t.Fatalf("redirect=%v want %v (Location %q)", got, tc.wantRedirect, rec.Header().Get("Location"))
			}
		})
	}
}
