package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// iconReq drives the icon handler the way the route does, with the
// identity a requireAuth-wrapped request carries — team t1's.
func iconReq(t *testing.T, s *Server, connector string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/v1/connectors/"+connector+"/icon", nil)
	r.SetPathValue("id", connector)
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{
		UserID: "viewer1", TeamID: "t1", Credential: auth.CredentialAccessToken,
	}))
	rec := httptest.NewRecorder()
	s.handleConnectorIcon(rec, r)
	return rec
}

// TestConnectorIconServesSanitizedBytesWithHeaders: the baked forgejo's
// icon comes out sanitized, with the icon content type, the sandbox CSP,
// nosniff, and the load's digest as a quoted ETag — and a matching
// If-None-Match is a 304 with no body.
func TestConnectorIconServesSanitizedBytesWithHeaders(t *testing.T) {
	s := newConnectionTestServer(t)
	rec := iconReq(t, s, "forgejo")
	if rec.Code != http.StatusOK {
		t.Fatalf("forgejo icon: code=%d body=%s", rec.Code, rec.Body.String())
	}
	pkg, _, err := s.connectorCatalog.Package("forgejo")
	if err != nil {
		t.Fatalf("baked forgejo: %v", err)
	}
	if rec.Body.String() != string(pkg.IconSVG) {
		t.Fatalf("served bytes are not the sanitized ones:\n%s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("content type %q", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "default-src 'none'; style-src 'unsafe-inline'; sandbox" {
		t.Fatalf("csp %q", csp)
	}
	if nosniff := rec.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Fatalf("nosniff %q", nosniff)
	}
	etag := rec.Header().Get("ETag")
	if etag != `"`+pkg.IconDigest+`"` {
		t.Fatalf("etag %q, want the load digest %q", etag, pkg.IconDigest)
	}
	r := httptest.NewRequest("GET", "/api/v1/connectors/forgejo/icon", nil)
	r.SetPathValue("id", "forgejo")
	r.Header.Set("If-None-Match", etag)
	rec304 := httptest.NewRecorder()
	r = r.WithContext(connUserCtx("viewer1"))
	s.handleConnectorIcon(rec304, r)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: code=%d", rec304.Code)
	}
}

// TestConnectorIconMonogramFallback: a package that ships no icon gets the
// monogram — its initials on its brand color, in the foreground that
// color's luminance picks — and the same bytes on every read.
func TestConnectorIconMonogramFallback(t *testing.T) {
	s := newConnectionTestServer(t)
	pkgs := connection.NewMemoryPackageStore()
	files := connectorPackageFiles(t, monogramProbePackage())
	if err := pkgs.Publish(nil, connection.StoredPackage{
		TenantID: "t1", ConnectorID: "probe", Files: files, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	s.connectorPackages = pkgs // the team tier, composed like the cloud does
	rec := iconReq(t, s, "probe")
	if rec.Code != http.StatusOK {
		t.Fatalf("probe icon: code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `fill="#ff9e1b"`) {
		t.Fatalf("monogram lacks the brand tile:\n%s", body)
	}
	if !strings.Contains(body, ">PR</text>") {
		t.Fatalf("monogram lacks the display-name initials:\n%s", body)
	}
	if !strings.Contains(body, `fill="#000000"`) {
		t.Fatalf("pale accent must take dark text:\n%s", body)
	}
	again := iconReq(t, s, "probe")
	if again.Body.String() != body || again.Header().Get("ETag") != rec.Header().Get("ETag") {
		t.Fatal("the monogram is not stable across reads")
	}
	sum := sha256.Sum256([]byte(body))
	if rec.Header().Get("ETag") != `"`+hex.EncodeToString(sum[:])[:16]+`"` {
		t.Fatalf("monogram etag %q is not of the served bytes", rec.Header().Get("ETag"))
	}
}

// TestConnectorIconWithoutACatalogPlaneIs503: a deployment that resolves
// no packages at all answers as an outage, not as a missing connector.
func TestConnectorIconWithoutACatalogPlaneIs503(t *testing.T) {
	s := &Server{}
	rec := iconReq(t, s, "forgejo")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no catalog plane: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestConnectorIconUnknownConnectorIs404: a name no tier holds is a typed
// not-found, not a blank tile.
func TestConnectorIconUnknownConnectorIs404(t *testing.T) {
	s := newConnectionTestServer(t)
	rec := iconReq(t, s, "no-such-connector")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown connector: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestMonogramForegroundChoosesByContrast: the luminance rule reads dark
// tiles in white and pale ones in black.
func TestMonogramForegroundChoosesByContrast(t *testing.T) {
	for _, c := range []struct{ brand, want string }{
		{"#000000", "#ffffff"},
		{"#ffffff", "#000000"},
		{"#ff9e1b", "#000000"},
		{"#2b303b", "#ffffff"},
	} {
		if got := monogramForeground(c.brand); got != c.want {
			t.Fatalf("foreground(%s) = %s, want %s", c.brand, got, c.want)
		}
	}
}

// monogramProbePackage is a catalog package that declares an accent and a
// display name but ships no icon — the monogram's exact case.
func monogramProbePackage() *spec.Package {
	pkg := teamProbePackage("https://probe.example")
	pkg.Connector.DisplayName = "Probe Two"
	pkg.Connector.BrandColor = "#ff9e1b"
	return pkg
}

// TestMonogramInitialsAreLettersOnly: a hostile or exotic display name
// contributes letter runes only — punctuation and markup never reach the
// served XML (a `"`, `&` or `<` in the name used to emit invalid SVG).
func TestMonogramInitialsAreLettersOnly(t *testing.T) {
	for _, c := range []struct{ display, id, want string }{
		{`"Ascend" AI`, "acme", "AS"},
		{"& x", "acme", "X"}, // the only letter the name holds
		{"< <Img>", "probe", "IM"},
		{"Acme & Co", "acme", "AC"},
		{"--//--", "acme", "AC"}, // no letters at all → the id's
	} {
		got := monogramInitials(c.display, c.id)
		if got != c.want {
			t.Fatalf("initials(%q, %q) = %q, want %q", c.display, c.id, got, c.want)
		}
	}
}
