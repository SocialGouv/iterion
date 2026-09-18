package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

// Three shapes the first version of the rewrite got wrong, each cheap and each
// producing a URL that names something other than what it should.
func TestTheRewriteHandlesTheAwkwardOrigins(t *testing.T) {
	const protoRelative = `<meta property="og:image" content="//cdn.example.com/a.png" />`
	fs := fstest.MapFS{"index.html": {Data: []byte(
		`<html><head>` + protoRelative + `<meta name="twitter:image" content="/brand/x.png" /></head></html>`)}}

	body := getIndex(t, SPAHandler(fs, "https://iterion.cloud"), "/")
	// A protocol-relative URL is ALREADY absolute. Prefixing an origin onto it
	// names the wrong host.
	if !strings.Contains(body, protoRelative) {
		t.Errorf("a protocol-relative image was rewritten:\n%s", body)
	}
	if !strings.Contains(body, `content="https://iterion.cloud/brand/x.png"`) {
		t.Errorf("the root-relative image beside it was not rewritten:\n%s", body)
	}

	// `$` is a legal URL sub-delimiter and a replacement-template expansion
	// marker. Unescaped, an origin carrying one ate the image path entirely.
	for _, base := range []string{"https://ex.com/$1", "https://ex.com/$", "https://ex.com/a$b"} {
		out := getIndex(t, SPAHandler(spaFixture(), base), "/")
		want := `content="` + base + `/brand/iterion-bot-circle.png"`
		if !strings.Contains(out, want) {
			t.Errorf("base %q: want %s in:\n%s", base, want, out)
		}
	}
}

// Every test above feeds the rewrite a fixture written to match it, which
// proves the regex does what it says and NOTHING about the template that
// actually ships. The pattern is attribute-order sensitive: `content=` moved
// ahead of `property=` — a reformat, a plugin, a hand edit — makes the rewrite
// a silent no-op and the card regresses to text-only with every test still
// green.
//
// So run the real function over the real file. No spellings enumerated: the
// input is the artefact, and the assertion is the crawler's requirement.
func TestTheShippedTemplateStillMatchesTheRewrite(t *testing.T) {
	const tmpl = "../../studio/index.html"
	raw, err := os.ReadFile(tmpl)
	if err != nil {
		t.Fatalf("read %s: %v", tmpl, err)
	}
	if !bytes.Contains(raw, []byte("og:image")) {
		t.Fatalf("%s declares no og:image at all — the card has no picture to absolutise", tmpl)
	}

	got := absolutiseSocialImages(raw, "https://iterion.cloud")
	if !bytes.Contains(got, []byte(`content="https://iterion.cloud/brand/`)) {
		t.Errorf("the rewrite did not reach %s's social image; the shipped card stays relative.\ngot:\n%s", tmpl, got)
	}
	if socialImageMeta.Match(got) {
		t.Errorf("%s still carries a root-relative social image after the rewrite", tmpl)
	}
}

// "/" used to be served by http.FileServer, which sent a Content-Length and
// answered a Range. Routing it through serveIndex for the rewrite must not
// quietly cost the length — a HEAD carried none at all.
func TestTheIndexCarriesItsLength(t *testing.T) {
	h := SPAHandler(spaFixture(), "https://iterion.cloud")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
		got := rec.Header().Get("Content-Length")
		if got == "" {
			t.Errorf("%s /: no Content-Length", method)
			continue
		}
		if method == http.MethodGet && got != strconv.Itoa(rec.Body.Len()) {
			t.Errorf("GET /: Content-Length %s but %d bytes written", got, rec.Body.Len())
		}
	}
}
