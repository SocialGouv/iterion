package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// BuildAssetPrefix is the bundler's `build.assetsDir`: every content-hashed
// chunk, stylesheet and font the build emits lands under it, and nothing else
// does. No client-side route starts with it.
const BuildAssetPrefix = "/assets/"

// IsBuildAssetPath reports whether p addresses a bundler-emitted artifact
// rather than a client-side route. Such a request must never receive the SPA
// shell. The browser asked for a script or a stylesheet; answering 200
// text/html makes it fail on the MIME type ("blocked due to a disallowed MIME
// type") instead of on the missing file, and the page dies with no usable
// diagnosis. A hashed asset is absent for exactly one reason — the document
// came from a different build than the replica answering this request — and
// 404 is the only answer that says so.
//
// p is cleaned here rather than at each call site. Most callers are bare
// handlers with no ServeMux in front of them, so the path arrives exactly as
// the client wrote it: `/x/<id>//assets/app.js` reaches the guard with the
// doubled slash a browser does not collapse, and an uncleaned prefix test lets
// it through to the SPA shell — the very bug this guard exists to close.
func IsBuildAssetPath(p string) bool {
	clean := path.Clean(p)
	return clean == strings.TrimSuffix(BuildAssetPrefix, "/") ||
		strings.HasPrefix(clean, BuildAssetPrefix)
}

// IsBuildAssetDir reports whether clean names a DIRECTORY under the build-asset
// prefix. http.FileServer answers such a path with a directory index — an HTML
// listing of every chunk and sourcemap the build emitted — so the invariant
// "under /assets/ you get a file or a 404" would be false exactly where it is
// easiest to reach.
//
// It is a shared helper rather than a guard copied into each handler: the two
// surfaces that delegate to http.FileServer sit two lines apart in different
// files, and the first round of this change guarded one and not the other.
func IsBuildAssetDir(sub fs.FS, clean string) bool {
	if !IsBuildAssetPath(clean) {
		return false
	}
	info, err := fs.Stat(sub, strings.TrimPrefix(path.Clean(clean), "/"))
	return err == nil && info.IsDir()
}

// NotFoundBuildAsset answers a request for a build artifact this build does not
// carry. Exported because every surface that serves the SPA needs the identical
// answer, including the desktop asset proxy in its own package.
//
// The header matters as much as the status: a content-addressed URL that
// 404s mid-rollout becomes VALID once the fleet converges, and a cached
// negative outlives the incident — re-fetching index.html yields the same hash
// and hits the same cached 404, which no client-side reload can repair.
func NotFoundBuildAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.NotFound(w, r)
}

// SPAHandler serves a Vite-style single-page app from sub: assets resolve to
// their real file, unknown paths fall back to index.html so client-side
// routes like /runs/abc render the shell instead of a hard 404. The fallback
// is gated on GET/HEAD, a non-/api/ prefix and a non-/assets/ prefix, so
// neither JSON endpoints nor build artifacts masquerade as HTML.
// publicURL, when non-empty, is the deployment's externally-reachable origin.
// It exists for ONE reason: rewriting the relative og:image in index.html into
// the absolute URL OpenGraph specifies (see absolutiseSocialImages).
func SPAHandler(sub fs.FS, publicURL string) http.Handler {
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)
		// Any /api/* request reaching this catch-all is a genuinely unrouted
		// endpoint (e.g. a cloud-only admin route on a local-mode server). It
		// must NOT fall back to the SPA shell: a JSON client would JSON.parse
		// the returned index.html and die with a cryptic "unexpected character
		// at line 1 column 1". Return an authoritative JSON 404 instead — this
		// is the /api/ gate this handler's doc comment always promised.
		if strings.HasPrefix(clean, "/api/") {
			httpError(w, http.StatusNotFound, "no such API endpoint: %s", clean)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fileServer.ServeHTTP(w, r)
			return
		}
		if clean == "/" || clean == "." {
			// Through serveIndex, not the file server: "/" is the product home,
			// and its social-card image has to be absolutised like every other
			// route's. http.FileServer would hand back the bytes untouched.
			serveIndex(w, r, sub, publicURL)
			return
		}
		if IsBuildAssetDir(sub, clean) {
			NotFoundBuildAsset(w, r)
			return
		}
		rel := strings.TrimPrefix(clean, "/")
		if f, err := sub.Open(rel); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		} else if !errors.Is(err, fs.ErrNotExist) {
			fileServer.ServeHTTP(w, r)
			return
		}
		if IsBuildAssetPath(clean) {
			NotFoundBuildAsset(w, r)
			return
		}
		serveIndex(w, r, sub, publicURL)
	})
}

// ServeInjectedIndex serves index.html after installing immutable bootstrap
// values in the page. It is shared by the desktop proxy and the browser
// workspace host so both surfaces use the same scoping contract.
func ServeInjectedIndex(w http.ResponseWriter, r *http.Request, sub fs.FS, scope string, workspace bool) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var bootstrap bytes.Buffer
	bootstrap.WriteString("<script>")
	if scope != "" {
		encoded, _ := json.Marshal(scope)
		bootstrap.WriteString("window.__ITERION_SCOPE__=")
		bootstrap.Write(encoded)
		bootstrap.WriteString(";")
	}
	if workspace {
		bootstrap.WriteString("window.__ITERION_WORKSPACE__=true;")
	}
	bootstrap.WriteString("</script>")
	inject := bootstrap.Bytes()
	if i := bytes.Index(data, []byte("<head>")); i >= 0 {
		out := make([]byte, 0, len(data)+len(inject))
		out = append(out, data[:i+len("<head>")]...)
		out = append(out, inject...)
		data = append(out, data[i+len("<head>"):]...)
	} else {
		data = append(inject, data...)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS, publicURL string) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data = absolutiseSocialImages(data, publicURL)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// Set explicitly: "/" used to come from http.FileServer, which sent one.
	// Without it the response is chunked and a HEAD carries no length at all.
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// socialImageMeta matches an og:image / twitter:image whose content is a
// root-relative path.
// The content must start with a single "/": a protocol-relative "//cdn/x.png"
// is ALREADY absolute, and prefixing an origin onto it produces a URL naming
// the wrong host.
var socialImageMeta = regexp.MustCompile(`(?i)(<meta\s+(?:property|name)="(?:og:image|twitter:image)"\s+content=")(/(?:[^/"][^"]*)?)(")`)

// absolutiseSocialImages rewrites a root-relative social-card image into the
// absolute URL OpenGraph specifies.
//
// index.html ships the path RELATIVE because one bundle serves iterion.cloud,
// a preprod host and every self-hosted deployment, so no build-time value is
// right for all three. But a relative og:image is dropped by the crawlers that
// matter — LinkedIn in particular — and the preview renders text-only. The
// server is the first place that knows its own origin, so it is where the two
// requirements can both be met.
//
// A no-op when publicURL is unset, which is every local and desktop run: there
// is no origin to name, nothing external crawls them, and the bytes are served
// exactly as built.
func absolutiseSocialImages(data []byte, publicURL string) []byte {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if base == "" {
		return data
	}
	// `$` is a legal sub-delimiter in a URL and an expansion marker in a
	// replacement template: an origin containing one silently ate the image
	// path. Doubling escapes it.
	base = strings.ReplaceAll(base, "$", "$$")
	return socialImageMeta.ReplaceAll(data, []byte("${1}"+base+"${2}${3}"))
}
