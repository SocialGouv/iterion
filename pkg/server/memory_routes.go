package server

import (
	"io"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/memory"
	"github.com/SocialGouv/iterion/pkg/store"
)

// memoryStore returns the configured shared-knowledge store, falling
// back to the local filesystem store when none was wired (local studio).
func (s *Server) memoryStore() knowledge.MemoryStore {
	if s.memStore != nil {
		return s.memStore
	}
	return memory.DefaultFSStore()
}

// registerMemoryRoutes wires the shared-knowledge REST surface. Spaces
// are addressed by query params; the tenant/user come from the auth
// identity (never a query param) so a member can't read another org's
// memory.
func (s *Server) registerMemoryRoutes() {
	s.mux.Handle("GET /api/memory/usage", s.requireAuth(http.HandlerFunc(s.handleMemoryUsage)))
	s.mux.Handle("GET /api/memory/docs", s.requireAuth(http.HandlerFunc(s.handleMemoryListDocs)))
	s.mux.Handle("GET /api/memory/doc", s.requireAuth(http.HandlerFunc(s.handleMemoryReadDoc)))
	s.mux.Handle("PUT /api/memory/doc", s.requireAuth(http.HandlerFunc(s.handleMemoryWriteDoc)))
	s.mux.Handle("DELETE /api/memory/doc", s.requireAuth(http.HandlerFunc(s.handleMemoryDeleteDoc)))
	s.mux.Handle("GET /api/memory/export", s.requireAuth(http.HandlerFunc(s.handleMemoryExport)))
	s.mux.Handle("POST /api/memory/import", s.requireAuth(http.HandlerFunc(s.handleMemoryImport)))
}

func (s *Server) memoryRef(r *http.Request) (knowledge.SpaceRef, bool) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		return knowledge.SpaceRef{}, false
	}
	vis := knowledge.Visibility(q.Get("visibility"))
	if vis == "" {
		vis = knowledge.VisibilityProject
	}
	botID := q.Get("bot")
	if vis == knowledge.VisibilityBot && botID == "" {
		return knowledge.SpaceRef{}, false
	}
	// Tenant/user are taken from the request identity (cloud); in local
	// mode they're empty → the store maps them to "local".
	tenant, _ := store.TenantFromContext(r.Context())
	owner, _ := store.OwnerFromContext(r.Context())
	ref := memory.ResolveSpaceRef(vis, name, botID, owner, memory.SpaceRefInputs{
		TenantID:  tenant,
		UserID:    owner,
		ProjectID: q.Get("project"),
		BotID:     botID,
	})
	if err := ref.Validate(); err != nil {
		return knowledge.SpaceRef{}, false
	}
	return ref, true
}

// requireMemoryWriteAuth gates a memory MUTATION. The `global` space is
// instance-wide (not tenant-scoped), so a write/delete/import there is
// visible to EVERY tenant — in multi-tenant (cloud) mode that requires a
// super-admin whose platform authority stands (platformAuthorityStands: the
// flag from the account, the credential still live). Tenant-scoped spaces
// only affect the caller's own org, so a member may write them. Local
// single-tenant mode (no identity store) has no cross-tenant concern and is
// always allowed.
func (s *Server) requireMemoryWriteAuth(w http.ResponseWriter, r *http.Request, ref knowledge.SpaceRef) bool {
	if ref.Visibility != knowledge.VisibilityGlobal {
		return true
	}
	if s.authStore() == nil {
		return true // local single-tenant
	}
	if s.platformAuthority(w, r) {
		return true
	}
	return false
}

// memoryDocPath reads and validates the ?path= param, writing a 400 and
// returning ok=false when it is missing or escapes the space.
func memoryDocPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	path := r.URL.Query().Get("path")
	if err := knowledge.ValidateDocPath(path); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return "", false
	}
	return path, true
}

func (s *Server) handleMemoryUsage(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.memoryRef(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid space (need ?name=, valid ?visibility=; visibility=bot also needs ?bot=)")
		return
	}
	used, quota, err := s.memoryStore().UsageBytes(r.Context(), ref)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, map[string]int64{"used_bytes": used, "quota_bytes": quota})
}

func (s *Server) handleMemoryListDocs(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.memoryRef(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid space")
		return
	}
	docs, err := s.memoryStore().ListDocuments(r.Context(), ref, r.URL.Query().Get("dir"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if docs == nil {
		docs = []knowledge.DocumentMeta{}
	}
	writeJSON(w, struct {
		Documents []knowledge.DocumentMeta `json:"documents"`
	}{Documents: docs})
}

func (s *Server) handleMemoryReadDoc(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.memoryRef(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid space")
		return
	}
	path, ok := memoryDocPath(w, r)
	if !ok {
		return
	}
	doc, err := s.memoryStore().ReadDocument(r.Context(), ref, path)
	if err != nil {
		httpError(w, http.StatusNotFound, "%s", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write(doc.Content)
}

func (s *Server) handleMemoryWriteDoc(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.memoryRef(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid space")
		return
	}
	if !s.requireMemoryWriteAuth(w, r, ref) {
		return
	}
	path, ok := memoryDocPath(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, knowledge.DefaultMaxDocumentSize+1))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	meta, err := s.memoryStore().WriteDocument(r.Context(), ref, knowledge.DocumentInput{Path: path, Content: body, UpdatedBy: identityActor(r)})
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%s", err.Error())
		return
	}
	writeJSON(w, meta)
}

func (s *Server) handleMemoryDeleteDoc(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.memoryRef(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid space")
		return
	}
	if !s.requireMemoryWriteAuth(w, r, ref) {
		return
	}
	path, ok := memoryDocPath(w, r)
	if !ok {
		return
	}
	if err := s.memoryStore().DeleteDocument(r.Context(), ref, path); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMemoryExport(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.memoryRef(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid space")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"memory-export.tar.gz\"")
	if _, err := knowledge.ExportSpace(r.Context(), s.memoryStore(), ref, w); err != nil {
		// Headers may already be flushed; best-effort log.
		if s.logger != nil {
			s.logger.Warn("memory export %s: %v", ref.ID(), err)
		}
	}
}

func (s *Server) handleMemoryImport(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.memoryRef(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid space")
		return
	}
	if !s.requireMemoryWriteAuth(w, r, ref) {
		return
	}
	strategy := knowledge.ImportStrategy(r.URL.Query().Get("strategy"))
	sum, err := knowledge.ImportSpace(r.Context(), s.memoryStore(), ref, r.Body, strategy)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%s", err.Error())
		return
	}
	writeJSON(w, sum)
}

// identityActor returns a "user_id" attribution for memory writes.
func identityActor(r *http.Request) string {
	if id, ok := auth.FromContext(r.Context()); ok {
		return id.UserID
	}
	return ""
}
