package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPAHandler serves a Vite-style single-page app from sub: assets resolve to
// their real file, unknown paths fall back to index.html so client-side
// routes like /runs/abc render the shell instead of a hard 404. The fallback
// is gated on GET/HEAD and a non-/api/ prefix so JSON endpoints stay
// authoritative 404s rather than masquerading as HTML.
func SPAHandler(sub fs.FS) http.Handler {
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
			fileServer.ServeHTTP(w, r)
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
		serveIndex(w, r, sub)
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

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
