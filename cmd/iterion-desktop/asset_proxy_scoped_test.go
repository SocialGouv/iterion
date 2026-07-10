//go:build desktop

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// registerConn is a tiny helper: install one open connection in the registry.
func withConn(app *App, c *activeConn) *App {
	if app.conns == nil {
		app.conns = map[string]*activeConn{}
	}
	app.conns[c.id] = c
	return app
}

// TestServeScoped_UnknownConn: a pane pointed at a closed/unknown connection
// must 404, never leak to some other backend.
func TestServeScoped_UnknownConn(t *testing.T) {
	h := &assetProxyHandler{app: &App{}}
	req := httptest.NewRequest(http.MethodGet, "/x/nope/api/server/info", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown conn status = %d, want 404", rec.Code)
	}
}

// TestServeScoped_LocalDemuxAndWsInfo: a local pane's /x/<id>/api/... reaches
// the backend with the scope stripped and NO Authorization; /x/<id>/_ws/info
// reports the ws base with needs_ticket=false.
func TestServeScoped_LocalDemuxAndWsInfo(t *testing.T) {
	var sawPath, sawAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	app := withConn(&App{}, &activeConn{id: "loc1", kind: ProjectKindLocal, serverURL: backend.URL + "/"})
	h := &assetProxyHandler{app: app}

	// Demux: /x/loc1/api/server/info → backend /api/server/info, no Bearer.
	req := httptest.NewRequest(http.MethodGet, "/x/loc1/api/server/info", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local demux status = %d, want 200", rec.Code)
	}
	if sawPath != "/api/server/info" {
		t.Errorf("backend saw path %q, want /api/server/info (scope not stripped?)", sawPath)
	}
	if sawAuth != "" {
		t.Errorf("local demux injected Authorization %q, want none", sawAuth)
	}

	// _ws/info: ws base derived from the backend http URL, no ticket for local.
	req = httptest.NewRequest(http.MethodGet, "/x/loc1/_ws/info", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("_ws/info status = %d, want 200", rec.Code)
	}
	var info connWsInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("_ws/info decode: %v", err)
	}
	if !strings.HasPrefix(info.WsBase, "ws://") {
		t.Errorf("_ws/info ws_base = %q, want ws:// prefix", info.WsBase)
	}
	if info.NeedsTicket {
		t.Errorf("_ws/info needs_ticket = true for a local connection, want false")
	}
}

// TestServeScoped_ScopedIndex: any non-api/non-_ws path under /x/<id>/ serves
// the SPA index with the scope injected as window.__ITERION_SCOPE__.
func TestServeScoped_ScopedIndex(t *testing.T) {
	app := withConn(&App{}, &activeConn{id: "loc9", kind: ProjectKindLocal, serverURL: "http://127.0.0.1:1/"})
	h := &assetProxyHandler{
		app:   app,
		subFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><head><title>x</title></head><body></body>")}},
	}

	req := httptest.NewRequest(http.MethodGet, "/x/loc9/runs/abc", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped index status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `window.__ITERION_SCOPE__="/x/loc9"`) {
		t.Errorf("scoped index missing injected scope; body head: %.120s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("scoped index Content-Type = %q, want text/html", ct)
	}
}

// TestServeScoped_CloudBearerAndTicket: a cloud pane's /x/<id>/api/... carries
// the injected Bearer, /_ws/info reports needs_ticket=true, and /_ws/ticket
// mints a single-use ticket via the jar's access token.
func TestServeScoped_CloudBearerAndTicket(t *testing.T) {
	tok := mkJWT(time.Now().Add(15 * time.Minute).Unix())
	var sawAuth string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/me":
			sawAuth = r.Header.Get("Authorization")
			if r.Header.Get("Authorization") != "Bearer "+tok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"u1"}`))
		case "/api/ws/ticket":
			if r.Header.Get("Authorization") != "Bearer "+tok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ticket":"T-123"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cloud.Close()

	jar := newCloudTokenJar("cl1", cloud.URL, newFakeStore())
	if err := jar.applyRotation(tok, "refresh-seed"); err != nil {
		t.Fatalf("seed jar: %v", err)
	}
	app := withConn(&App{}, &activeConn{id: "cl1", kind: ProjectKindCloud, serverURL: cloud.URL + "/", jar: jar})
	h := &assetProxyHandler{app: app}

	// Demux with Bearer injection.
	req := httptest.NewRequest(http.MethodGet, "/x/cl1/api/auth/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cloud demux status = %d, want 200 (Bearer not injected?)", rec.Code)
	}
	if sawAuth != "Bearer "+tok {
		t.Errorf("cloud saw Authorization %q, want injected Bearer", sawAuth)
	}

	// _ws/info reports needs_ticket for cloud.
	req = httptest.NewRequest(http.MethodGet, "/x/cl1/_ws/info", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var info connWsInfo
	_ = json.Unmarshal(rec.Body.Bytes(), &info)
	if !info.NeedsTicket {
		t.Errorf("cloud _ws/info needs_ticket = false, want true")
	}
	if !strings.HasPrefix(info.WsBase, "ws://") {
		t.Errorf("cloud _ws/info ws_base = %q, want ws:// prefix", info.WsBase)
	}

	// _ws/ticket mints via the jar.
	req = httptest.NewRequest(http.MethodPost, "/x/cl1/_ws/ticket", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("_ws/ticket status = %d, want 200", rec.Code)
	}
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tk); err != nil {
		t.Fatalf("_ws/ticket decode: %v", err)
	}
	if tk.Ticket != "T-123" {
		t.Errorf("_ws/ticket = %q, want T-123", tk.Ticket)
	}
}
