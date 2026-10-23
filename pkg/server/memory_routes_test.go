package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestMemoryRoutes_RoundTrip(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	s := New(Config{SkipProjectRegistration: true}, iterlog.New(iterlog.LevelError, nil))
	ctx := context.Background()
	const space = "visibility=org&name=conv"

	// write
	w := httptest.NewRecorder()
	s.handleMemoryWriteDoc(w, httptest.NewRequest("PUT", "/api/memory/doc?"+space+"&path=a.md", strings.NewReader("# A")).WithContext(ctx))
	if w.Code != http.StatusOK {
		t.Fatalf("write: %d %s", w.Code, w.Body.String())
	}

	// read
	w = httptest.NewRecorder()
	s.handleMemoryReadDoc(w, httptest.NewRequest("GET", "/api/memory/doc?"+space+"&path=a.md", nil).WithContext(ctx))
	if w.Code != http.StatusOK || w.Body.String() != "# A" {
		t.Fatalf("read: %d %q", w.Code, w.Body.String())
	}

	// usage
	w = httptest.NewRecorder()
	s.handleMemoryUsage(w, httptest.NewRequest("GET", "/api/memory/usage?"+space, nil).WithContext(ctx))
	var u map[string]int64
	json.Unmarshal(w.Body.Bytes(), &u)
	if u["used_bytes"] != 3 {
		t.Fatalf("usage: %v", u)
	}

	// list
	w = httptest.NewRecorder()
	s.handleMemoryListDocs(w, httptest.NewRequest("GET", "/api/memory/docs?"+space, nil).WithContext(ctx))
	var lr struct {
		Documents []knowledge.DocumentMeta `json:"documents"`
	}
	json.Unmarshal(w.Body.Bytes(), &lr)
	if len(lr.Documents) != 1 || lr.Documents[0].Path != "a.md" {
		t.Fatalf("list: %+v", lr.Documents)
	}

	// export → non-empty gzip
	w = httptest.NewRecorder()
	s.handleMemoryExport(w, httptest.NewRequest("GET", "/api/memory/export?"+space, nil).WithContext(ctx))
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("export: %d len=%d", w.Code, w.Body.Len())
	}

	// missing name → 400
	w = httptest.NewRecorder()
	s.handleMemoryUsage(w, httptest.NewRequest("GET", "/api/memory/usage?visibility=org", nil).WithContext(ctx))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing name should 400, got %d", w.Code)
	}

	// traversal path → 400 (clamp)
	for _, bad := range []string{"../evil", "/abs", "a/../../x"} {
		w = httptest.NewRecorder()
		s.handleMemoryWriteDoc(w, httptest.NewRequest("PUT", "/api/memory/doc?"+space+"&path="+bad, strings.NewReader("x")).WithContext(ctx))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("path %q should 400, got %d", bad, w.Code)
		}
	}
}

// TestMemoryRoutes_GlobalWriteGate: in multi-tenant (authStore set) mode,
// writing the instance-global space requires super-admin; a member is
// rejected. Local mode (no authStore) is covered by the round-trip test.
func TestMemoryRoutes_GlobalWriteGate(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	s := newOrgTestServer(t) // authSvc set → s.authStore() != nil
	const gl = "visibility=global&name=shared"

	// Dev-mode principals: the gate's derivation reads the flag they carry,
	// as a server without authentication would stamp them.
	member := auth.WithIdentity(context.Background(), auth.Identity{UserID: "m", TeamID: "t1", Credential: auth.CredentialDevMode})
	w := httptest.NewRecorder()
	s.handleMemoryWriteDoc(w, httptest.NewRequest("PUT", "/api/memory/doc?"+gl+"&path=a.md", strings.NewReader("# A")).WithContext(member))
	if w.Code != http.StatusForbidden {
		t.Fatalf("member global write should 403, got %d (%s)", w.Code, w.Body.String())
	}

	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "admin", IsSuperAdmin: true, Credential: auth.CredentialDevMode})
	w = httptest.NewRecorder()
	s.handleMemoryWriteDoc(w, httptest.NewRequest("PUT", "/api/memory/doc?"+gl+"&path=a.md", strings.NewReader("# A")).WithContext(admin))
	if w.Code != http.StatusOK {
		t.Fatalf("super-admin global write should 200, got %d (%s)", w.Code, w.Body.String())
	}

	// A tenant-scoped space (org) is NOT gated — a member may write it.
	w = httptest.NewRecorder()
	s.handleMemoryWriteDoc(w, httptest.NewRequest("PUT", "/api/memory/doc?visibility=org&name=team&path=a.md", strings.NewReader("# A")).WithContext(member))
	if w.Code != http.StatusOK {
		t.Fatalf("member org write should 200, got %d (%s)", w.Code, w.Body.String())
	}
}
