package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tenantGuardStore mimics the mongo store's tenant filter at the
// LoadRunCtx boundary: a run is only visible to its owning tenant. It
// embeds a real (empty) filesystem store so the runview.Service can
// construct (reconcileOrphans walks it at startup) while LoadRunCtx is
// overridden to enforce the tenant gate.
type tenantGuardStore struct {
	*store.FilesystemRunStore
	runTenant string
}

func (g *tenantGuardStore) LoadRunCtx(ctx context.Context, id string) (*store.Run, error) {
	if tid, ok := store.TenantFromContext(ctx); ok && tid != g.runTenant {
		return nil, errors.New("store: run file not found")
	}
	return &store.Run{ID: id, TenantID: g.runTenant}, nil
}

// A caller from another tenant must not be able to cancel/pause/read a
// run by guessing its id: each handler now pre-checks ownership via
// LoadRunCtx (which carries the request's tenant), so cross-tenant
// requests get 404 before any mutation or filesystem read.
func TestRunEndpointsRejectCrossTenant(t *testing.T) {
	srv, _ := newTestServer(t)
	orig := srv.runs
	t.Cleanup(func() { srv.runs = orig }) // restore so Shutdown drains the real service

	realStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc, err := runview.NewService(srv.cfg.StoreDir, runview.WithStore(&tenantGuardStore{FilesystemRunStore: realStore, runTenant: "tenant-A"}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv.runs = svc // handlers deref s.runs at call time

	cases := []struct {
		name     string
		method   string
		path     string
		call     func(http.ResponseWriter, *http.Request)
		pathVals map[string]string
	}{
		{"cancel", http.MethodPost, "/api/runs/run-1/cancel", srv.handleCancelRun, map[string]string{"id": "run-1"}},
		{"pause", http.MethodPost, "/api/runs/run-1/pause", srv.handlePauseRun, map[string]string{"id": "run-1"}},
		{"log", http.MethodGet, "/api/runs/run-1/log", srv.handleGetRunLog, map[string]string{"id": "run-1"}},
		{"toolblob", http.MethodGet, "/api/runs/run-1/tools/tu-1/input", srv.handleGetToolBlob, map[string]string{"id": "run-1", "toolUseID": "tu-1", "kind": "input"}},
		{"artifact-files-list", http.MethodGet, "/api/runs/run-1/artifact-files", srv.handleListArtifactFiles, map[string]string{"id": "run-1"}},
		{"artifact-file-get", http.MethodGet, "/api/runs/run-1/artifact-files/report.md", srv.handleGetArtifactFile, map[string]string{"id": "run-1", "path": "report.md"}},
		{"file-preview", http.MethodGet, "/api/runs/run-1/files/preview/image.png", srv.handleGetRunFilePreview, map[string]string{"id": "run-1", "path": "image.png"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			for k, v := range c.pathVals {
				req.SetPathValue(k, v)
			}
			// Caller is tenant-B; the run belongs to tenant-A.
			req = req.WithContext(store.WithTenant(req.Context(), "tenant-B"))
			rec := httptest.NewRecorder()
			c.call(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s cross-tenant: got status %d, want 404", c.name, rec.Code)
			}
		})
	}
}
