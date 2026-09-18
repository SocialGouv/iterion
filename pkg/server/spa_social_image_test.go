package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// The index as it ships: the social-card image is RELATIVE, because one bundle
// serves iterion.cloud, preprod and every self-hosted deployment.
const indexWithRelativeCard = `<!DOCTYPE html>
<html lang="en"><head>
<title>Iterion</title>
<meta name="description" content="Build, run and orchestrate agentic AI workflows." />
<meta property="og:image" content="/brand/iterion-bot-circle.png" />
<meta name="twitter:image" content="/brand/iterion-bot-circle.png" />
<link rel="icon" href="/favicon.ico" />
</head><body><div id="root"></div></body></html>`

func spaFixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte(indexWithRelativeCard)},
		"assets/app-a1.js": {Data: []byte("console.log(1)")},
	}
}

func getIndex(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, rec.Code)
	}
	return rec.Body.String()
}

// OpenGraph specifies an absolute URL, and LinkedIn drops a relative one — the
// preview then renders text-only. The server is the first place that knows its
// own origin, so it is the only place both requirements can be met.
func TestTheSocialCardImageIsAbsoluteWhenTheOriginIsKnown(t *testing.T) {
	h := SPAHandler(spaFixture(), "https://iterion.cloud")

	// "/" is the product home and reaches the index through a different arm of
	// the handler than a deep link does. Both must rewrite: the home is the
	// page people actually share.
	for _, path := range []string{"/", "/studio/runs/abc"} {
		body := getIndex(t, h, path)
		if strings.Contains(body, `content="/brand/`) {
			t.Errorf("GET %s still serves a relative social image:\n%s", path, body)
		}
		for _, want := range []string{
			`<meta property="og:image" content="https://iterion.cloud/brand/iterion-bot-circle.png" />`,
			`<meta name="twitter:image" content="https://iterion.cloud/brand/iterion-bot-circle.png" />`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: missing %s", path, want)
			}
		}
	}
}

// Everything that is NOT a social image must come through untouched. A rewrite
// on the index path of every mode is worth exactly as much as its blast
// radius is small.
func TestTheRewriteTouchesNothingElse(t *testing.T) {
	body := getIndex(t, SPAHandler(spaFixture(), "https://iterion.cloud"), "/")
	for _, untouched := range []string{
		`<link rel="icon" href="/favicon.ico" />`,
		`<meta name="description" content="Build, run and orchestrate agentic AI workflows." />`,
		`<title>Iterion</title>`,
		`<div id="root"></div>`,
	} {
		if !strings.Contains(body, untouched) {
			t.Errorf("the rewrite altered %s\ngot:\n%s", untouched, body)
		}
	}
}

// Local and desktop runs have no origin to name, nothing external crawls them,
// and a trailing slash on a configured one must not double.
func TestNoOriginMeansNoRewrite(t *testing.T) {
	for _, base := range []string{"", "   "} {
		body := getIndex(t, SPAHandler(spaFixture(), base), "/")
		if body != indexWithRelativeCard {
			t.Errorf("base %q rewrote the index; it must be served byte-for-byte", base)
		}
	}
	body := getIndex(t, SPAHandler(spaFixture(), "https://iterion.cloud/"), "/")
	if strings.Contains(body, "cloud//brand") {
		t.Errorf("a trailing slash on the public URL doubled:\n%s", body)
	}
}

// The guard sits on the index path of every mode, so a request for a real
// asset must not pay for it — nor be corrupted by it.
func TestBuildAssetsAreNotRewritten(t *testing.T) {
	rec := httptest.NewRecorder()
	SPAHandler(spaFixture(), "https://iterion.cloud").
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-a1.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Fatalf("asset served as %d %q", rec.Code, rec.Body.String())
	}
}

// The wiring, not a hand-passed string. The tests above build the handler
// themselves, which proves the rewrite works and says NOTHING about whether
// the server hands it its own origin — the exact shape of a green test over a
// dead capability. This one goes through the Server: it registers the static
// routes the way the cloud path does and asks the mux.
//
// `iterion studio` never sets PublicURL (it runs through cli.RunStudio, a
// different builder), which is why a local probe shows no rewrite and is
// right to. The deployment that has crawlers is `iterion server`, whose
// server.Config carries PublicURL from ITERION_PUBLIC_URL.
func TestTheServerHandsTheSPAItsOwnOrigin(t *testing.T) {
	s := &Server{
		mux: newRecordingMux(),
		cfg: Config{PublicURL: "https://iterion.cloud"},
	}
	// The real registration function, not a hand-built handler.
	s.mountSPA(spaFixture())

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status %d", rec.Code)
	}
	const want = `content="https://iterion.cloud/brand/iterion-bot-circle.png"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the server did not pass its own PublicURL to the SPA handler; want %s in:\n%s", want, rec.Body.String())
	}
}
