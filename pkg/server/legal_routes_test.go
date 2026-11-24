package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/legal"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The legal pages are DEPLOYMENT CONFIGURATION (ADR-160): a document the
// operator configured is served public, one they did not is a 404 and no
// link, and the surface is read by a visitor with no session at all.

func legalTestSurface(t *testing.T) legal.Surface {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mentions-legales.md", "# Mentions légales\n\nÉditeur : Fabrique numérique.")
	write("accessibilite.md", "# Déclaration d'accessibilité\n\nÉtat : non conforme.")
	docs, err := legal.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return legal.Surface{
		Documents:     docs,
		ContactEmail:  "contact@example.org",
		Accessibility: legal.Accessibility{Status: legal.StatusNonCompliant, Date: "2026-11-03"},
	}
}

// newAuthedCloudLegalServer is an auth-enabled cloud server (no DisableAuth)
// behind the auth middleware, so the public-path bypass is what admits the
// anonymous reads below.
func newAuthedCloudLegalServer(t *testing.T, surface legal.Surface) (*Server, http.Handler) {
	t.Helper()
	srv := New(Config{
		Mode:                    "cloud",
		WorkDir:                 t.TempDir(),
		Legal:                   surface,
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, nil))
	return srv, srv.authMiddleware(srv.mux)
}

func getLegal(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "203.0.113.7:4242"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLegalDocument_ConfiguredIsServedPublic(t *testing.T) {
	_, h := newAuthedCloudLegalServer(t, legalTestSurface(t))
	rec := getLegal(t, h, "/api/legal/mentions-legales", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous GET /api/legal/mentions-legales = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got legalDocumentView
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Slug != legal.SlugLegalNotice || got.Title != "Mentions légales" {
		t.Errorf("slug/title = %q/%q", got.Slug, got.Title)
	}
	if !strings.Contains(got.Markdown, "Éditeur : Fabrique numérique.") {
		t.Errorf("markdown = %q, want the configured text", got.Markdown)
	}
	if got.ContactEmail != "contact@example.org" {
		t.Errorf("contact_email = %q", got.ContactEmail)
	}
	if got.Accessibility.Status != legal.StatusNonCompliant || got.Accessibility.Date != "2026-11-03" {
		t.Errorf("accessibility = %+v", got.Accessibility)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a legal document")
	}
	if rec := getLegal(t, h, "/api/legal/mentions-legales", map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Errorf("revalidation with the ETag = %d, want 304", rec.Code)
	}
	// An ingress that gzips weakens the tag (W/"…"), and a browser may send
	// a list: both revalidate with a 304, never a second body.
	for _, inm := range []string{"W/" + etag, `"other", ` + etag} {
		if rec := getLegal(t, h, "/api/legal/mentions-legales", map[string]string{"If-None-Match": inm}); rec.Code != http.StatusNotModified {
			t.Errorf("revalidation with If-None-Match %s = %d, want 304", inm, rec.Code)
		}
	}
}

func TestLegalDocument_NotConfiguredIs404(t *testing.T) {
	_, h := newAuthedCloudLegalServer(t, legalTestSurface(t))
	// A known slug the deployment did not configure.
	rec := getLegal(t, h, "/api/legal/cgu", nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not published") {
		t.Errorf("GET /api/legal/cgu = %d %s, want 404 'not published'", rec.Code, rec.Body.String())
	}
	// A slug that names no document at all.
	rec = getLegal(t, h, "/api/legal/terms", nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no such legal document") {
		t.Errorf("GET /api/legal/terms = %d %s, want 404 'no such legal document'", rec.Code, rec.Body.String())
	}
}

func TestLegalDocument_NothingConfiguredIs404ForEverySlug(t *testing.T) {
	_, h := newAuthedCloudLegalServer(t, legal.Surface{})
	for _, slug := range legal.Slugs {
		if rec := getLegal(t, h, "/api/legal/"+string(slug), nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/legal/%s on an unconfigured deployment = %d, want 404", slug, rec.Code)
		}
	}
}

func TestLegalDocument_IsRateLimited(t *testing.T) {
	srv, h := newAuthedCloudLegalServer(t, legalTestSurface(t))
	if srv.authLimiter == nil {
		t.Fatal("no rate limiter wired")
	}
	var last int
	for i := 0; i < int(legalReadLimit.burst)+1; i++ {
		last = getLegal(t, h, "/api/legal/accessibilite", nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("request %d from one IP = %d, want 429", int(legalReadLimit.burst)+1, last)
	}
}

// The legal reads spend a bucket of their own: the limiter refills a bucket
// at its caller's rate, so a generous anonymous GET sharing the bare-IP
// bucket of the login family would hand an attacker login attempts back.
func TestLegalDocument_RateLimitDoesNotRefillTheLoginBudget(t *testing.T) {
	srv, h := newAuthedCloudLegalServer(t, legalTestSurface(t))
	now := time.Unix(1_700_000_000, 0)
	limiter := newAuthRateLimiter()
	limiter.now = func() time.Time { return now }
	srv.authLimiter = limiter

	probe := httptest.NewRequest(http.MethodGet, "/api/legal/accessibilite", nil)
	probe.RemoteAddr = "203.0.113.7:4242"
	ipKey := srv.clientIP(probe)
	login := authBucketCfg{rate: 1.0 / 12.0, burst: 5} // auth_routes.go loginLimit
	for i := 0; i < int(login.burst); i++ {
		if ok, _ := limiter.allow(ipKey, login); !ok {
			t.Fatalf("login attempt %d refused before the budget was spent", i+1)
		}
	}
	if ok, _ := limiter.allow(ipKey, login); ok {
		t.Fatal("login budget not spent after its burst")
	}

	now = now.Add(10 * time.Second)
	if rec := getLegal(t, h, "/api/legal/accessibilite", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/legal/accessibilite = %d, want 200", rec.Code)
	}
	if ok, _ := limiter.allow(ipKey, login); ok {
		t.Fatal("a legal read refilled the login budget of its IP: the legal bucket must not share the bare-IP key")
	}
}

func TestServerInfo_LegalBlockListsConfiguredDocumentsOnly(t *testing.T) {
	_, h := newAuthedCloudLegalServer(t, legalTestSurface(t))
	rec := getLegal(t, h, "/api/server/info", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/server/info = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	var info struct {
		Legal legalInfoBlock `json:"legal"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Legal.ContactEmail != "contact@example.org" || info.Legal.Accessibility.Status != legal.StatusNonCompliant {
		t.Errorf("legal = %+v", info.Legal)
	}
	var slugs []string
	for _, d := range info.Legal.Documents {
		slugs = append(slugs, string(d.Slug))
	}
	if strings.Join(slugs, ",") != "mentions-legales,accessibilite" {
		t.Errorf("documents = %v, want the two configured ones in slug order", slugs)
	}

	// Nothing configured: an empty list, never null — the SPA iterates it.
	_, h = newAuthedCloudLegalServer(t, legal.Surface{})
	body, _ = io.ReadAll(getLegal(t, h, "/api/server/info", nil).Body)
	if !strings.Contains(string(body), `"documents":[]`) {
		t.Errorf("unconfigured legal block = %s, want \"documents\":[]", body)
	}
}

func TestIsPublicPathLegal(t *testing.T) {
	for _, p := range []string{"/api/legal/cgu", "/api/legal/mentions-legales"} {
		if !isPublicPath(p) {
			t.Errorf("isPublicPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/api/legal", "/api/legalx/cgu"} {
		if isPublicPath(p) {
			t.Errorf("isPublicPath(%q) = true, want false", p)
		}
	}
}
