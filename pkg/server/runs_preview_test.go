package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The low-level public-unicast / host-resolution unit tests moved to
// pkg/secure/httpdial (where the logic now lives, shared with the OIDC SSO
// connectors and completion webhooks). The handler-level SSRF guard below
// stays here because it exercises the server's preview-proxy wiring end-to-end.

// TestHandlePreviewProxy_RejectsPrivateTargetStrict exercises the SSRF guard
// end-to-end through the HTTP handler in strict (cloud) mode: private,
// loopback and cloud-metadata targets must be refused with 403 before any
// connection is attempted, malformed schemes/hosts with 400, and an unknown
// run id with 404 (relay prevention).
func TestHandlePreviewProxy_RejectsPrivateTargetStrict(t *testing.T) {
	srv, hs := newTestServer(t)
	srv.cfg.Mode = "cloud" // force strict SSRF validation regardless of bind
	seedRun(t, srv, "run-ssrf", "wf", store.RunStatusRunning)

	get := func(t *testing.T, runID, target string) int {
		t.Helper()
		u := hs.URL + "/api/runs/" + runID + "/preview?target=" + url.QueryEscape(target)
		resp, err := http.Get(u) // #nosec G107 G704 — test URL targets the local httptest server
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("metadata endpoint rejected (403)", func(t *testing.T) {
		if got := get(t, "run-ssrf", "http://169.254.169.254/latest/meta-data/"); got != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", got)
		}
	})
	t.Run("private rfc1918 rejected (403)", func(t *testing.T) {
		if got := get(t, "run-ssrf", "http://192.168.0.1/"); got != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", got)
		}
	})
	t.Run("loopback rejected (403)", func(t *testing.T) {
		if got := get(t, "run-ssrf", "http://127.0.0.1:9/"); got != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", got)
		}
	})
	t.Run("non-http scheme rejected (400)", func(t *testing.T) {
		if got := get(t, "run-ssrf", "file:///etc/passwd"); got != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", got)
		}
	})
	t.Run("missing target rejected (400)", func(t *testing.T) {
		// Empty target is indistinguishable from an omitted one: the handler's
		// `target == ""` check rejects both with 400.
		if got := get(t, "run-ssrf", ""); got != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", got)
		}
	})
	t.Run("unknown run rejected before dial (404)", func(t *testing.T) {
		if got := get(t, "does-not-exist", "http://8.8.8.8/"); got != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", got)
		}
	})
}

// #2337: the preview fetch never consults an ambient proxy — with one, the
// pinned dial would connect to the run-chosen target while net/http wrote a
// proxy-form request for the PROXY, handing the target the operator's
// Proxy-Authorization. Reverting previewUpstreamTransport to an inline
// transport with http.ProxyFromEnvironment turns this red.
func TestPreviewUpstreamTransport_NeverProxies(t *testing.T) {
	tr := previewUpstreamTransport("127.0.0.1:1")
	if tr.Proxy != nil {
		t.Fatal("preview upstream transport has a proxy func — an ambient proxy would steer, and credential-mark, a guarded fetch")
	}
	if tr.DialContext == nil {
		t.Fatal("preview upstream transport lost its pinned dial")
	}
}
