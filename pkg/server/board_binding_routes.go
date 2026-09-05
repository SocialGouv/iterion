package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
)

// The team ⇄ project-board binding endpoints (ADR-097 §4).
//
// GET is a member read; PUT and DELETE need team-manage rights: binding a
// board redirects where a team's work is projected, and rebinding it silently
// would be a way to point somebody else's roadmap at your own board.

// registerBoardBindingRoutes mounts the endpoints. Like every other forge
// surface they self-disable when their stores are absent (local mode), so a
// self-hosted studio simply does not advertise them.
func (s *Server) registerBoardBindingRoutes() {
	if s.boardBindings == nil {
		return
	}
	s.mux.Handle("GET /api/teams/{id}/board-binding", s.requireAuth(http.HandlerFunc(s.handleGetBoardBinding)))
	s.mux.Handle("PUT /api/teams/{id}/board-binding", s.requireAuth(http.HandlerFunc(s.handlePutBoardBinding)))
	s.mux.Handle("DELETE /api/teams/{id}/board-binding", s.requireAuth(http.HandlerFunc(s.handleDeleteBoardBinding)))
}

// boardBindingReq is the PUT payload: the board's ADDRESS plus the policy.
// Never the resolved ids — a caller that could name a project id could point a
// team at a board it has no rights on, so every id in the stored binding comes
// from reading the board with the team's own credential.
type boardBindingReq struct {
	Owner        string `json:"owner"`
	OwnerKind    string `json:"owner_kind,omitempty"` // org (default) | user
	Number       int    `json:"number"`
	ConnectionID string `json:"connection_id"`
	// StatusMap overrides the shipped five-column vocabulary
	// (`{"Todo": "ready"}`). Must be injective.
	StatusMap map[string]string `json:"status_map,omitempty"`
	// SyncEverySeconds is the reconciliation interval. Absent = the default;
	// an explicit 0 = off. A pointer, because those are different answers.
	SyncEverySeconds *int64 `json:"sync_every_seconds,omitempty"`
}

func (s *Server) handleGetBoardBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	b, err := s.boardBindings.GetByTenant(r.Context(), teamID)
	if err != nil {
		if errors.Is(err, forge.ErrBoardBindingNotFound) {
			httpError(w, http.StatusNotFound, "no project board bound to this team")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, b)
}

func (s *Server) handlePutBoardBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req boardBindingReq
	if !decodeJSON(w, r, &req) {
		return
	}
	bind, err := s.bindTeamBoard(r.Context(), teamID, req)
	if err != nil {
		// Every failure here is the caller's request or their board: a bad
		// ref, a non-injective map, an interval under the floor, a project the
		// credential cannot see. 400 with the reason beats a 500 that hides
		// which of those it was.
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := s.boardBindings.Upsert(r.Context(), bind); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	stored, err := s.boardBindings.GetByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, stored)
}

func (s *Server) handleDeleteBoardBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if err := s.boardBindings.Delete(r.Context(), teamID); err != nil {
		if errors.Is(err, forge.ErrBoardBindingNotFound) {
			httpError(w, http.StatusNotFound, "no project board bound to this team")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bindTeamBoard resolves a request into a binding by READING the board with
// the connection's credential. It is the shared body behind the HTTP handler
// and any other caller (the remote CLI goes through the HTTP one).
func (s *Server) bindTeamBoard(ctx context.Context, teamID string, req boardBindingReq) (forge.BoardBinding, error) {
	// The team id is authorized from the PATH; the connection id arrives in the
	// BODY, and ConnectionStore.Get is keyed on the id alone with no tenant
	// filter — so an id belonging to another team resolves perfectly well.
	// Checking ownership here is therefore this route's own responsibility, as
	// it is for every peer forge route (connAdminFor, the launch path, the
	// approval routes).
	//
	// Skipping it is not a read leak that ends with the response: the binding
	// is PERSISTED with that connection, so the sync worker keeps using another
	// org's credential indefinitely — and it WRITES, calling SetSingleSelect on
	// that org's board. The refusal is non-enumerating: "not found" for both a
	// foreign connection and a nonexistent one, so a caller cannot probe which.
	conn, err := s.connectionOwnedBy(ctx, teamID, req.ConnectionID)
	if err != nil {
		return forge.BoardBinding{}, err
	}
	if err := s.assertProjectGrant(ctx, conn); err != nil {
		return forge.BoardBinding{}, err
	}
	bc, provider, err := s.boardClientFor(ctx, req.ConnectionID)
	if err != nil {
		return forge.BoardBinding{}, err
	}
	bind := forge.BindRequest{
		TenantID: teamID,
		Provider: provider,
		Ref: forge.ProjectRef{
			Owner:     req.Owner,
			OwnerKind: forge.ProjectOwnerKind(req.OwnerKind),
			Number:    req.Number,
		},
		ConnectionID: req.ConnectionID,
		StatusMap:    req.StatusMap,
	}
	if req.SyncEverySeconds != nil {
		d := time.Duration(*req.SyncEverySeconds) * time.Second
		bind.SyncEvery = &d
	}
	return forge.BindBoard(ctx, bc, bind)
}

// assertProjectGrant refuses a bind whose GitHub-App credential does not hold
// the org-level projects grant, NAMING it.
//
// It has to run before BindBoard because the symptom is indistinguishable from
// the operator's own typo: GitHub answers a project the token cannot see with
// NOT_FOUND, which `GetProject` maps to forge.ErrProjectNotFound, so without
// this probe the most likely first-run failure of the whole feature reads as
// "project not found: SocialGouv/203" and sends the operator to re-check a
// board number that was right all along. The grant is org-scoped: only an org
// owner can approve it, so the message has to say WHICH one.
//
// Only App connections are probed. A PAT's scopes are not readable from the
// API, so for those the board read stays the only oracle.
func (s *Server) assertProjectGrant(ctx context.Context, conn forge.Connection) error {
	if conn.Kind != forge.KindGitHubApp {
		return nil
	}
	granted, err := s.installationGrantsFor(ctx, conn)
	if err != nil {
		// The probe is a DIAGNOSTIC, not the gate — BindBoard's own read is.
		// Refusing here would turn an unreachable /app/installations into a
		// second way to fail a bind that would have worked. Say so, then let
		// the board read answer.
		if s.logger != nil {
			s.logger.Warn("board binding: could not read installation %d grants for connection %s: %v",
				conn.InstallationID, conn.ID, err)
		}
		return nil
	}
	missing := forgegithub.MissingProjectPermissions(granted)
	if len(missing) == 0 {
		// Also covers an unknown grant set: absence of data is not evidence of
		// a gap (MissingProjectPermissions returns nothing for an empty map).
		return nil
	}
	return fmt.Errorf("this GitHub App installation is missing the %s permission, which project boards need — "+
		"add it to the App (Organization permissions → Projects: Read and write), have an org owner approve the "+
		"new grant on the installation, then bind again", strings.Join(missing, ", "))
}

// installationGrantsFor reads what the installation's owner actually approved,
// LIVE. The copy stored on the connection is a cache refreshed by the health
// and refresh routes, so binding on it would refuse a board an owner approved
// the grant for five minutes ago — the live read is the authority, the stored
// one the fallback when there is no App config to sign with.
func (s *Server) installationGrantsFor(ctx context.Context, conn forge.Connection) (map[string]string, error) {
	if s.forgeInstallationGrants != nil {
		return s.forgeInstallationGrants(ctx, conn)
	}
	cfg, _, ok := s.githubAppConfigForConnection(ctx, conn)
	if !ok || conn.InstallationID == 0 {
		return conn.GrantedPermissions, nil
	}
	inst, err := forgegithub.InstallationInfo(ctx, s.forgeHTTPClient(),
		forgegithub.APIBaseFor(conn.BaseURL()), cfg, conn.InstallationID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return inst.Permissions, nil
}

// errConnectionNotOwned is the single non-enumerating refusal both the
// foreign-connection and the unknown-connection cases return.
var errConnectionNotOwned = errors.New("connection not found")

// connectionOwnedBy resolves a connection AND refuses one that does not belong
// to teamID. It returns the record so a caller needing both the boundary and
// the credential's facts reads the store once.
//
// It fails CLOSED when the connection store is absent: a deployment with no
// connection store cannot prove ownership, and a credential boundary that
// degrades to "allow" when its evidence is missing is not a boundary.
func (s *Server) connectionOwnedBy(ctx context.Context, teamID, connID string) (forge.Connection, error) {
	if s.forgeConnections == nil {
		return forge.Connection{}, errors.New("forge connections are not configured on this instance")
	}
	conn, err := s.forgeConnections.Get(ctx, connID)
	if err != nil || conn.TenantID != teamID {
		return forge.Connection{}, errConnectionNotOwned
	}
	return conn, nil
}

// assertConnectionOwnedBy is connectionOwnedBy for the callers that only need
// the verdict.
func (s *Server) assertConnectionOwnedBy(ctx context.Context, teamID, connID string) error {
	_, err := s.connectionOwnedBy(ctx, teamID, connID)
	return err
}

// boardClientFor resolves a forge connection into a project-board client. The
// indirection through a field is what lets a test inject a fake without a
// connection store, and what keeps the cloud/local difference in ONE place.
func (s *Server) boardClientFor(ctx context.Context, connID string) (forge.BoardClient, forge.Provider, error) {
	if s.boardClientForConnection != nil {
		return s.boardClientForConnection(ctx, connID)
	}
	if s.forgeConnections == nil {
		return nil, "", errors.New("forge connections are not configured on this instance")
	}
	conn, err := s.forgeConnections.Get(ctx, connID)
	if err != nil {
		return nil, "", err
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return nil, "", err
	}
	bc, ok := forge.AsBoardClient(admin)
	if !ok {
		return nil, "", errors.New("this connection's provider exposes no project board")
	}
	return bc, conn.Provider, nil
}

// boardClientForBoundBinding resolves the board client a bound team's sync
// pass writes through — the worker's BoardClientFor.
//
// It re-asserts ownership at USE time, not only at write time. The bind path
// guarantees the stored connection belonged to the team when it was written;
// this covers what happens afterwards — a connection deleted, or re-created
// under another tenant — so a pass that WRITES to a forge can never ride a
// credential the team no longer owns. The worker turns the refusal into its
// per-tenant warning and skips that team.
func (s *Server) boardClientForBoundBinding(ctx context.Context, b forge.BoardBinding) (forge.BoardClient, error) {
	if s.forgeConnections != nil {
		if err := s.assertConnectionOwnedBy(ctx, b.TenantID, b.ConnectionID); err != nil {
			return nil, err
		}
	}
	if s.boardClientForBinding != nil {
		return s.boardClientForBinding(ctx, b)
	}
	bc, _, err := s.boardClientFor(ctx, b.ConnectionID)
	return bc, err
}

// boardProjection builds the reflect's fast path (the trigger spine's
// projection effect), or nil when this instance has nothing to reflect onto.
//
// It shares the sync worker's resolvers on purpose: the two paths must run the
// same reflect, through the same credential and the same card store, or "the
// fast path wrote it" and "the pass would have written it" stop meaning the
// same thing.
func (s *Server) boardProjection() *boardProjectionEffect {
	if s == nil || s.boardBindings == nil || s.cfg.CloudBoardFor == nil {
		return nil
	}
	return &boardProjectionEffect{
		Bindings:       s.boardBindings,
		BoardClientFor: s.boardClientForBoundBinding,
		CardsFor:       s.cloudCardsFor,
		Logger:         s.logger,
	}
}

// cloudCardsFor resolves a tenant's cloud card store — the one resolver the
// sync worker and the projection effect share.
func (s *Server) cloudCardsFor(_ context.Context, tenantID string) (native.BoardStore, error) {
	b := s.cfg.CloudBoardFor(tenantID)
	if b == nil {
		return nil, errors.New("no card store for this team")
	}
	return b, nil
}

// startBoardSync launches the periodic reconciliation worker (ADR-097 §10).
// It needs a binding store AND a per-tenant card store; without either there
// is nothing to reconcile, and saying so once at boot beats a worker that
// ticks forever over an empty set.
func (s *Server) startBoardSync() {
	if s == nil || s.boardBindings == nil || s.cfg.CloudBoardFor == nil {
		return
	}
	w := &BoardSyncWorker{
		Bindings:       s.boardBindings,
		BoardClientFor: s.boardClientForBoundBinding,
		CardsFor:       s.cloudCardsFor,
		Logger:         s.logger,
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.boardSyncCancel = cancel
	errtrack.Go("server.boardSync", func() { w.Run(ctx) })
	if s.logger != nil {
		s.logger.Info("server: project-board reconciliation started (per-team interval, elected per tenant)")
	}
}
