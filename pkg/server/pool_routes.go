package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/google/uuid"
)

// The key pools' control surface (ADR-164): named pools owned by a user,
// an org or the platform; their ENTRIES (the pledges a pool serves); and
// the per-scope ATTACHMENTS that opt a cascade level into one pool. Three
// precedents cover every requirement: the price-feed routes (per-scope
// CRUD with a loud CAS 409, role gates, audit), the byok routes
// (write-only secrets, masked views), and the admin llm routes (the
// instance tier). Two laws shape the authority model:
//
//   - Law 2 (zero pool without opt-in): writing an ATTACHMENT needs BOTH
//     the level's own write right AND the pool's consent — its owner, or
//     an Audience that admits the level.
//   - Ownership manages the pool: the owner trio is decided at creation
//     and never patched; a user manages their pools, an org admin its
//     org's, a super-admin the platform's.

func (s *Server) registerPoolsRoutes() {
	// Pool CRUD, per owner scope.
	s.mux.Handle("POST /api/me/pools", s.requireAuth(http.HandlerFunc(s.handleCreateMyPool)))
	s.mux.Handle("GET /api/me/pools", s.requireAuth(http.HandlerFunc(s.handleListMyPools)))
	s.mux.Handle("POST /api/orgs/{id}/pools", s.requireAuth(http.HandlerFunc(s.handleCreateOrgPool)))
	s.mux.Handle("GET /api/orgs/{id}/pools", s.requireAuth(http.HandlerFunc(s.handleListOrgPools)))
	s.mux.Handle("POST /api/admin/llm/pools", s.requireSuperAdmin(http.HandlerFunc(s.handleCreatePlatformPool)))
	s.mux.Handle("GET /api/admin/llm/pools", s.requireSuperAdmin(http.HandlerFunc(s.handleListPlatformPools)))
	s.mux.Handle("PUT /api/pools/{id}", s.requireAuth(http.HandlerFunc(s.handlePutPool)))
	s.mux.Handle("DELETE /api/pools/{id}", s.requireAuth(http.HandlerFunc(s.handleDeletePool)))
	// Entries.
	s.mux.Handle("PUT /api/pools/{id}/entries", s.requireAuth(http.HandlerFunc(s.handlePutPoolEntry)))
	s.mux.Handle("GET /api/pools/{id}/entries", s.requireAuth(http.HandlerFunc(s.handleListPoolEntries)))
	s.mux.Handle("DELETE /api/pools/{id}/entries/{pledge}", s.requireAuth(http.HandlerFunc(s.handleDeletePoolEntry)))
	s.mux.Handle("POST /api/pools/{id}/entries/import", s.requireAuth(http.HandlerFunc(s.handleImportPoolEntry)))
	s.mux.Handle("POST /api/pools/{id}/entries/{pledge}/reset", s.requireAuth(http.HandlerFunc(s.handleResetPoolEntry)))
	// Attachments, one per cascade level.
	s.mux.Handle("GET /api/admin/llm/pool-attachment", s.requireSuperAdmin(http.HandlerFunc(s.handleGetPlatformPoolAttachment)))
	s.mux.Handle("PUT /api/admin/llm/pool-attachment", s.requireSuperAdmin(http.HandlerFunc(s.handlePutPlatformPoolAttachment)))
	s.mux.Handle("DELETE /api/admin/llm/pool-attachment", s.requireSuperAdmin(http.HandlerFunc(s.handleDeletePlatformPoolAttachment)))
	s.mux.Handle("GET /api/orgs/{id}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handleGetOrgPoolAttachment)))
	s.mux.Handle("PUT /api/orgs/{id}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handlePutOrgPoolAttachment)))
	s.mux.Handle("DELETE /api/orgs/{id}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handleDeleteOrgPoolAttachment)))
	s.mux.Handle("GET /api/teams/{id}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handleGetTeamPoolAttachment)))
	s.mux.Handle("PUT /api/teams/{id}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handlePutTeamPoolAttachment)))
	s.mux.Handle("DELETE /api/teams/{id}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handleDeleteTeamPoolAttachment)))
	s.mux.Handle("GET /api/teams/{id}/bots/{bot}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handleGetBotPoolAttachment)))
	s.mux.Handle("PUT /api/teams/{id}/bots/{bot}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handlePutBotPoolAttachment)))
	s.mux.Handle("DELETE /api/teams/{id}/bots/{bot}/pool-attachment", s.requireAuth(http.HandlerFunc(s.handleDeleteBotPoolAttachment)))
	// The manual reconciler pass — the onboarding proof.
	s.mux.Handle("POST /api/admin/llm/pools/sync", s.requireSuperAdmin(http.HandlerFunc(s.handlePoolsSync)))
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

type namedPoolView struct {
	ID         string            `json:"id"`
	OrgID      string            `json:"org_id,omitempty"`
	Name       string            `json:"name,omitempty"`
	Enabled    bool              `json:"enabled"`
	OwnerType  string            `json:"owner_type"`
	OwnerID    string            `json:"owner_id,omitempty"`
	Visibility string            `json:"visibility"`
	Audience   credpool.Audience `json:"audience"`
	UpdatedAt  *time.Time        `json:"updated_at,omitempty"`
}

func poolViewOf(p credpool.Pool) namedPoolView {
	return namedPoolView{
		ID: p.ID, OrgID: p.OrgID, Name: p.Name, Enabled: p.Enabled,
		OwnerType:  string(p.EffectiveOwnerType()),
		OwnerID:    p.OwnerID,
		Visibility: string(p.EffectiveVisibility()),
		Audience:   p.Audience,
		UpdatedAt:  &p.UpdatedAt,
	}
}

type poolEntryView struct {
	ID            string          `json:"id"`
	PoolID        string          `json:"pool_id"`
	Lender        string          `json:"lender"`
	Source        string          `json:"source"`
	Ref           string          `json:"ref"`
	KeyID         string          `json:"key_id,omitempty"`
	Backend       string          `json:"backend,omitempty"`
	Model         string          `json:"model,omitempty"`
	Priority      int             `json:"priority,omitempty"`
	Enabled       bool            `json:"enabled"`
	Limits        credpool.Limits `json:"limits"`
	Status        string          `json:"status"`
	Health        string          `json:"health"`
	HealthDetail  string          `json:"health_detail,omitempty"`
	CooldownUntil *time.Time      `json:"cooldown_until,omitempty"`
}

// poolEntryViewOf masks the entry: the credential itself never appears —
// the payload lives sealed in the oauth store or the api-key row, and the
// view carries identity (source/ref), the entry's route binding and the
// DERIVED availability status (the same vocabulary the launch path skips
// on), never a secret.
func poolEntryViewOf(p credpool.Pledge) poolEntryView {
	ok, status := p.Available(time.Now().UTC(), "")
	_ = ok
	health := string(p.Health)
	if health == "" {
		health = string(credpool.HealthOK)
	}
	return poolEntryView{
		ID: p.ID, PoolID: p.PoolID, Lender: p.UserID,
		Source: string(p.Source), Ref: p.Ref, KeyID: p.KeyID,
		Backend: p.Backend, Model: p.Model, Priority: p.Priority,
		Enabled: p.Enabled, Limits: p.Limits,
		Status: string(status), Health: health, HealthDetail: p.HealthDetail,
		CooldownUntil: p.CooldownUntil,
	}
}

type attachmentView struct {
	ScopeID   string              `json:"scope_id"`
	PoolID    string              `json:"pool_id"`
	Enabled   bool                `json:"enabled"`
	Strategy  credpool.Strategy   `json:"strategy"`
	Limits    credpool.DrawLimits `json:"limits"`
	UpdatedAt *time.Time          `json:"updated_at,omitempty"`
}

func attachmentViewOf(a credpool.Attachment) attachmentView {
	return attachmentView{ScopeID: a.ScopeID, PoolID: a.PoolID, Enabled: a.Enabled,
		Strategy: a.Strategy, Limits: a.Limits, UpdatedAt: &a.UpdatedAt}
}

// ---------------------------------------------------------------------------
// Pool CRUD
// ---------------------------------------------------------------------------

type poolCreateRequest struct {
	Name       string             `json:"name,omitempty"`
	Enabled    *bool              `json:"enabled,omitempty"`
	Visibility string             `json:"visibility,omitempty"`
	Audience   *credpool.Audience `json:"audience,omitempty"`
}

func (s *Server) handleCreateMyPool(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.createPool(w, r, credpool.Pool{
		OwnerType: credpool.PoolOwnerUser, OwnerID: id.UserID, Visibility: credpool.PoolVisibilityPrivate,
	})
}

func (s *Server) handleCreateOrgPool(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "org admin or owner required")
		return
	}
	s.createPool(w, r, credpool.Pool{OwnerType: credpool.PoolOwnerOrg, OwnerID: orgID, OrgID: orgID})
}

func (s *Server) handleCreatePlatformPool(w http.ResponseWriter, r *http.Request) {
	s.createPool(w, r, credpool.Pool{OwnerType: credpool.PoolOwnerPlatform})
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request, base credpool.Pool) {
	if s.credPoolPools == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "credential pools are not wired on this deployment")
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req poolCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	base.Name = strings.TrimSpace(req.Name)
	if req.Enabled != nil {
		base.Enabled = *req.Enabled
	}
	if req.Visibility != "" {
		base.Visibility = credpool.PoolVisibility(req.Visibility)
	}
	if req.Audience != nil {
		base.Audience = *req.Audience
	}
	if err := base.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%s", err)
		return
	}
	// The id is the server's mint: user input never names a store key.
	base.ID = uuid.NewString()
	if err := s.credPoolPools.Upsert(r.Context(), base); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "create pool: %v", err)
		return
	}
	s.auditPoolWrite(r, base, "created")
	s.writeJSONFor(w, r, poolViewOf(base))
}

func (s *Server) handleListMyPools(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.listPools(w, r, func(p credpool.Pool) bool {
		return p.EffectiveOwnerType() == credpool.PoolOwnerUser && p.OwnerID == id.UserID
	})
}

func (s *Server) handleListOrgPools(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this org")
		return
	}
	s.listPools(w, r, func(p credpool.Pool) bool {
		return p.EffectiveOwnerType() == credpool.PoolOwnerOrg && p.OrgID == orgID
	})
}

func (s *Server) handleListPlatformPools(w http.ResponseWriter, r *http.Request) {
	s.listPools(w, r, func(p credpool.Pool) bool { return p.EffectiveOwnerType() == credpool.PoolOwnerPlatform })
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request, keep func(credpool.Pool) bool) {
	if s.credPoolPools == nil {
		s.writeJSONFor(w, r, []namedPoolView{})
		return
	}
	pools, err := s.credPoolPools.ListAll(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list pools: %v", err)
		return
	}
	out := make([]namedPoolView, 0, len(pools))
	for _, p := range pools {
		if keep(p) {
			out = append(out, poolViewOf(p))
		}
	}
	s.writeJSONFor(w, r, out)
}

// poolManageOrFail loads a pool and answers whether the caller manages it:
// its owner, an admin of its org, or a super-admin. The ONE authority
// read every pool-scoped write shares.
func (s *Server) poolManageOrFail(w http.ResponseWriter, r *http.Request) (credpool.Pool, auth.Identity, bool) {
	id, _ := auth.FromContext(r.Context())
	if s.credPoolPools == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "credential pools are not wired on this deployment")
		return credpool.Pool{}, id, false
	}
	pool, err := s.credPoolPools.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pool not found")
		return pool, id, false
	}
	switch pool.EffectiveOwnerType() {
	case credpool.PoolOwnerUser:
		if pool.OwnerID == id.UserID {
			return pool, id, true
		}
	case credpool.PoolOwnerOrg:
		if pool.OrgID != "" && s.canManageOrg(r.Context(), id, pool.OrgID) {
			return pool, id, true
		}
	case credpool.PoolOwnerPlatform:
		if s.isSuperAdmin(r) {
			return pool, id, true
		}
	}
	s.httpErrorFor(w, r, http.StatusForbidden, "the pool's owner (or its org's admin) manages it")
	return pool, id, false
}

func (s *Server) handlePutPool(w http.ResponseWriter, r *http.Request) {
	pool, _, ok := s.poolManageOrFail(w, r)
	if !ok {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req poolPutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	// Patch-not-replace: the fields the request names move, the rest stay —
	// a concurrent writer's columns survive. The owner trio is NOT
	// patchable (decided at creation); a move is a new pool with the
	// entries re-pledged.
	patch := credpool.PoolPatch{}
	if req.Name != nil {
		patch.Name = req.Name
	}
	if req.Enabled != nil {
		patch.Enabled = req.Enabled
	}
	if req.Audience != nil {
		patch.Audience = req.Audience
	}
	if req.Visibility != nil {
		v := credpool.PoolVisibility(*req.Visibility)
		patch.Visibility = &v
	}
	if patch.IsEmpty() {
		s.httpErrorFor(w, r, http.StatusBadRequest, "the request names no field")
		return
	}
	updated, err := s.credPoolPools.Patch(r.Context(), pool.ID, patch)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "patch pool: %v", err)
		return
	}
	if err := updated.Validate(); err != nil {
		// The patched row must stay honest — a visibility/audience combo
		// validation refuses rolls the whole write back.
		if _, rerr := s.credPoolPools.Patch(r.Context(), pool.ID, poolPatchFrom(pool)); rerr != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "pool patch left an invalid row and the rollback failed: %v (was: %v)", rerr, err)
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "%s", err)
		return
	}
	s.auditPoolWrite(r, updated, "updated")
	s.writeJSONFor(w, r, poolViewOf(updated))
}

// poolPatchFrom writes every mutable field back — the rollback patch a
// refused pool write carries.
func poolPatchFrom(p credpool.Pool) credpool.PoolPatch {
	name := p.Name
	enabled := p.Enabled
	aud := p.Audience
	vis := p.Visibility
	return credpool.PoolPatch{Name: &name, Enabled: &enabled, Audience: &aud, Visibility: &vis}
}

type poolPutRequest struct {
	Name       *string            `json:"name,omitempty"`
	Enabled    *bool              `json:"enabled,omitempty"`
	Visibility *string            `json:"visibility,omitempty"`
	Audience   *credpool.Audience `json:"audience,omitempty"`
}

func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	pool, _, ok := s.poolManageOrFail(w, r)
	if !ok {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	entries, err := s.credPoolPledges.ListByPool(r.Context(), pool.ID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list entries: %v", err)
		return
	}
	if len(entries) > 0 {
		s.httpErrorFor(w, r, http.StatusConflict, "the pool holds %d entries — remove them first (a cascade delete would strand lenders' pledges silently)", len(entries))
		return
	}
	// An attachment still naming this pool must go first: a pool an
	// attachment resolves to refuses launches the moment it vanishes —
	// deliberate at the fold, a surprise mid-operation here.
	atts, err := s.credPoolAttachments.List(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list attachments: %v", err)
		return
	}
	for _, a := range atts {
		if a.PoolID == pool.ID {
			s.httpErrorFor(w, r, http.StatusConflict, "an attachment at %s still names this pool — detach it first", a.ScopeID)
			return
		}
	}
	if err := s.credPoolPools.Delete(r.Context(), pool.ID); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "delete pool: %v", err)
		return
	}
	s.auditPoolWrite(r, pool, "deleted")
	s.writeJSONFor(w, r, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------------
// Entries — the pledges a named pool serves. The lender is the pool's
// owner identity: a user pool's owner, or the reserved-scope sentinel the
// org/platform tiers push under (OrgTierOwnerKey / PlatformOwnerKey) —
// the same trick that lets the OAuth refresh worker and the fingerprint
// flows work on operator-pushed credentials without a schema change.
// ---------------------------------------------------------------------------

type poolEntryRequest struct {
	Source      string           `json:"source"`
	Ref         string           `json:"ref"`
	KeyID       string           `json:"key_id,omitempty"`
	Enabled     *bool            `json:"enabled,omitempty"`
	Limits      *credpool.Limits `json:"limits,omitempty"`
	Window      *credpool.Window `json:"window,omitempty"`
	Bots        []string         `json:"bots,omitempty"`
	FallbackUse bool             `json:"fallback_use,omitempty"`
	Backend     string           `json:"backend,omitempty"`
	Model       string           `json:"model,omitempty"`
	Priority    *int             `json:"priority,omitempty"`
	// LendScope is ACCEPTED on the wire and IGNORED: the scope is derived
	// from the pool's owner (poolLendScope). A request-named scope would
	// let a pool manager aim an entry at a foreign reserved scope (the
	// platform sentinel, another org's orgtier) and lend keys it was never
	// granted — the field exists so an old client does not 400; its value
	// never reaches a row.
	LendScope string `json:"lend_scope,omitempty"`
}

// poolLenderKey resolves the identity an entry's credential lives under.
func poolLenderKey(pool credpool.Pool) string {
	switch pool.EffectiveOwnerType() {
	case credpool.PoolOwnerOrg:
		return secrets.OrgTierOwnerKey(pool.OrgID)
	case credpool.PoolOwnerPlatform:
		return secrets.PlatformOwnerKey
	default:
		return pool.OwnerID
	}
}

// poolLendScope derives the reserved scope an entry reads its credential
// from, from the POOL's owner — never from the request. An org pool reads
// its org-tier rows, a platform pool the platform sentinel, a user pool
// the lender's personal keys (no reserved scope). The derived value and
// poolLenderKey agree by construction: one owner scope, one read.
func poolLendScope(pool credpool.Pool) string {
	switch pool.EffectiveOwnerType() {
	case credpool.PoolOwnerOrg:
		return secrets.OrgTierTenantID(pool.OrgID)
	case credpool.PoolOwnerPlatform:
		return secrets.PlatformTenantID
	default:
		return ""
	}
}

func (s *Server) handlePutPoolEntry(w http.ResponseWriter, r *http.Request) {
	pool, _, ok := s.poolManageOrFail(w, r)
	if !ok {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req poolEntryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	src := credpool.CredentialSource(req.Source)
	if !src.Valid() || strings.TrimSpace(req.Ref) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "unknown credential source %q or empty ref", req.Source)
		return
	}
	lender := poolLenderKey(pool)
	p := credpool.Pledge{
		ID:         credpool.PledgeID(lender, src, req.Ref),
		PoolID:     pool.ID,
		UserID:     lender,
		Credential: credpool.Credential{Source: src, Ref: req.Ref, KeyID: strings.TrimSpace(req.KeyID)},
		Enabled:    true, Health: credpool.HealthOK,
		Backend: req.Backend, Model: req.Model,
		// LendScope is DERIVED from the pool's owner scope, never taken
		// from the request: a request-named scope would let any pool
		// manager aim an entry at a foreign reserved scope (platform,
		// another org's orgtier) and lend keys it was never granted —
		// the lend consent is the pool's ownership, read here.
		LendScope: poolLendScope(pool),
		Bots:      req.Bots, FallbackUse: req.FallbackUse,
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}
	if req.Limits != nil {
		p.Limits = *req.Limits
	}
	if req.Window != nil {
		p.Window = req.Window
	}
	if req.Priority != nil {
		p.Priority = *req.Priority
	}
	if err := p.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%s", err)
		return
	}
	// The entry must reference a credential the lender actually HOLDS in
	// the scope it names — refusing here (not storing an inert pledge) is
	// what stops a silent entry.
	if err := s.credPool.VerifyEntry(r.Context(), p); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "the entry names no credential its lender holds: %s", err)
		return
	}
	// One lender lends one credential to ONE pool: the deterministic id
	// would otherwise Upsert the entry out of the pool it already serves,
	// silently re-homing it (the first pool's surface shows it gone with
	// no audit). Delete-first is the mover's explicit gesture.
	if existing, gerr := s.credPoolPledges.Get(r.Context(), p.ID); gerr == nil && existing.PoolID != pool.ID {
		s.httpErrorFor(w, r, http.StatusConflict, "this credential is already an entry of pool %s — remove it there first (one lender lends one credential to one pool)", existing.PoolID)
		return
	}
	if err := s.credPoolPledges.Upsert(r.Context(), p); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "put entry: %v", err)
		return
	}
	s.auditPoolWrite(r, pool, "entry.updated")
	s.writeJSONFor(w, r, poolEntryViewOf(p))
}

func (s *Server) handleListPoolEntries(w http.ResponseWriter, r *http.Request) {
	pool, _, ok := s.poolManageOrFail(w, r)
	if !ok {
		return
	}
	entries, err := s.credPoolPledges.ListByPool(r.Context(), pool.ID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list entries: %v", err)
		return
	}
	out := make([]poolEntryView, 0, len(entries))
	for _, e := range entries {
		out = append(out, poolEntryViewOf(e))
	}
	s.writeJSONFor(w, r, out)
}

func (s *Server) handleDeletePoolEntry(w http.ResponseWriter, r *http.Request) {
	pool, _, ok := s.poolManageOrFail(w, r)
	if !ok {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	// The pledge must BELONG to the pool the path names: pledge ids are
	// deterministic (lender|source|ref), so without this check anyone
	// managing their own pool could delete another pool's entries by id
	// (the reset route has carried the same guard since landing).
	p, err := s.credPoolPledges.Get(r.Context(), r.PathValue("pledge"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "entry not found")
		return
	}
	if p.PoolID != pool.ID {
		s.httpErrorFor(w, r, http.StatusNotFound, "entry not found in this pool")
		return
	}
	if err := s.credPoolPledges.Delete(r.Context(), p.ID); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "entry not found")
		return
	}
	s.auditPoolWrite(r, pool, "entry.deleted")
	s.writeJSONFor(w, r, map[string]string{"status": "deleted"})
}

type poolImportRequest struct {
	Source          string `json:"source"`
	CredentialsJSON string `json:"credentials_json"`
	Model           string `json:"model,omitempty"`
	Priority        *int   `json:"priority,omitempty"`
}

// handleImportPoolEntry is the operator's bulk-push lane (ADR-164): an
// oauth credentials blob (the claude credentials.json shape, or the codex
// auth.json paste) sealed under the pool's lender key, then pledged. A
// shape with no funding lane (an own-config-only harness, ADR-161
// decision 5) is stored INERT with a named reason — warn-not-reject,
// never a silent loss.
func (s *Server) handleImportPoolEntry(w http.ResponseWriter, r *http.Request) {
	pool, _, ok := s.poolManageOrFail(w, r)
	if !ok {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req poolImportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	if strings.TrimSpace(req.CredentialsJSON) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "credentials_json is required — the blob the local agent config carries")
		return
	}
	kind := secrets.OAuthKind(strings.TrimSpace(req.Source))
	if kind != secrets.OAuthKindClaudeCode && kind != secrets.OAuthKindCodex {
		s.httpErrorFor(w, r, http.StatusBadRequest, "import accepts the claude_code and codex subscription blobs; %q has no funding lane on this deployment (ADR-161 decision 5)", req.Source)
		return
	}
	lender := poolLenderKey(pool)
	// The blob is stored VERBATIM (the shape the runner's forfait dir
	// consumer reads) and sealed under the lender's key — never logged,
	// never returned.
	sealed, err := secrets.SealOAuthPayload(s.sealer, lender, kind, []byte(req.CredentialsJSON))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "seal subscription: %v", err)
		return
	}
	rec := secrets.OAuthRecord{
		UserID: lender, Kind: kind, SealedPayload: sealed,
		Fingerprint: secrets.FingerprintSHA256(req.CredentialsJSON + lender),
	}
	if err := s.oauthStore.Upsert(r.Context(), rec); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "store subscription: %v", err)
		return
	}
	p := credpool.Pledge{
		ID:         credpool.PledgeID(lender, credpool.SourceOAuth, string(kind)),
		PoolID:     pool.ID,
		UserID:     lender,
		Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: string(kind)},
		Enabled:    true, Health: credpool.HealthOK,
		Model: req.Model,
	}
	if req.Priority != nil {
		p.Priority = *req.Priority
	}
	if err := p.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%s", err)
		return
	}
	if existing, gerr := s.credPoolPledges.Get(r.Context(), p.ID); gerr == nil && existing.PoolID != pool.ID {
		s.httpErrorFor(w, r, http.StatusConflict, "this credential is already an entry of pool %s — remove it there first (one lender lends one credential to one pool)", existing.PoolID)
		return
	}
	if err := s.credPoolPledges.Upsert(r.Context(), p); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "put entry: %v", err)
		return
	}
	s.auditPoolWrite(r, pool, "entry.imported")
	s.writeJSONFor(w, r, poolEntryViewOf(p))
}

// handleResetPoolEntry is the MANUAL reset (ADR-164 § resets): clears the
// provider cooldown, forgets the stale readings the margin skip was acting
// on, and lifts every reset_hold naming the entry (the runs holding it
// re-rank at their next resume). The provider's own window may not have
// reset — the reset says "try it", and the next refusal re-cooldowns
// honestly.
func (s *Server) handleResetPoolEntry(w http.ResponseWriter, r *http.Request) {
	pool, _, ok := s.poolManageOrFail(w, r)
	if !ok {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	p, err := s.credPoolPledges.Get(r.Context(), r.PathValue("pledge"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "entry not found")
		return
	}
	if p.PoolID != pool.ID {
		s.httpErrorFor(w, r, http.StatusNotFound, "entry not found in this pool")
		return
	}
	// Re-read then write (the markUnhealthy doctrine): the copy may be
	// stale against a cooldown a finishing run just set.
	p, err = s.credPoolPledges.Get(r.Context(), p.ID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "entry not found")
		return
	}
	p.CooldownUntil = nil
	p.Health = credpool.HealthOK
	p.HealthDetail = ""
	p.ConsecutiveAuthFailures = 0
	if err := s.credPoolPledges.Upsert(r.Context(), p); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "reset entry: %v", err)
		return
	}
	if s.usageCaps != nil {
		// The documented operator escape hatch: the readings the margin
		// skip was acting on are forgotten, whatever their freshness said.
		if _, err := s.usageCaps.DeleteByFingerprint(r.Context(), s.credFingerprintOf(p)); err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "forget readings: %v", err)
			return
		}
	}
	if s.credPool != nil {
		if _, err := s.credPool.ClearHolds(r.Context(), p.ID); err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "lift holds: %v", err)
			return
		}
	}
	s.auditPoolWrite(r, pool, "entry.reset")
	s.writeJSONFor(w, r, poolEntryViewOf(p))
}

// credFingerprintOf names the audit identity a reset forgets readings for:
// the oauth record's stamp, or the key row's.
func (s *Server) credFingerprintOf(p credpool.Pledge) string {
	if p.Source == credpool.SourceOAuth && p.Ref != "" {
		if rec, err := s.oauthStore.Get(context.Background(), p.UserID, secrets.OAuthKind(p.Ref)); err == nil {
			return rec.Fingerprint
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Attachments — the per-level opt-in (law 2). Writing one needs the
// level's own write right AND the pool's consent: its owner, or an
// Audience that admits the level. Enabled=false is a resolved-disabled
// answer (the fold stops there); DELETE detaches and the level inherits.
// ---------------------------------------------------------------------------

type poolAttachmentRequest struct {
	PoolID            string     `json:"pool_id"`
	Enabled           *bool      `json:"enabled,omitempty"`
	Mode              string     `json:"mode,omitempty"`
	MarginPct         *float64   `json:"margin_pct,omitempty"`
	Triggers          []string   `json:"triggers,omitempty"`
	ResetHold         bool       `json:"reset_hold,omitempty"`
	MaxRunsPerDay     int        `json:"max_runs_per_day,omitempty"`
	MaxUSDPerDay      float64    `json:"max_usd_per_day,omitempty"`
	ExpectedUpdatedAt *time.Time `json:"expected_updated_at,omitempty"`
}

// poolAttachmentConsent is the pool half of the authority: the writer
// owns the pool, or its Audience admits the scope (org/team the level
// names, requester's user id for the reciprocity dial's future use).
func (s *Server) poolAttachmentConsent(r *http.Request, id auth.Identity, pool credpool.Pool, orgID, teamID, scopeID string) bool {
	// A PLATFORM pool consents through its OWN admins' write at the
	// PLATFORM scope: the instance floor is the platform pool's purpose
	// (the R&D onboarding recipe), the route is super-admin-gated, and the
	// write IS the explicit consent — never audience-implied, and never at
	// a lower scope (an org/team attachment of the instance pool goes
	// through the audience, whose zero value admits nobody).
	if pool.EffectiveOwnerType() == credpool.PoolOwnerPlatform && scopeID == credpool.ScopePlatform && s.isSuperAdmin(r) {
		return true
	}
	if pool.EffectiveOwnerType() == credpool.PoolOwnerUser && pool.OwnerID == id.UserID {
		return true
	}
	if pool.EffectiveOwnerType() == credpool.PoolOwnerOrg && pool.OrgID == orgID && orgID != "" {
		return true
	}
	return pool.Audience.Allows(pool.OrgID, orgID, teamID, id.UserID, false)
}

func (s *Server) getPoolAttachment(w http.ResponseWriter, r *http.Request, scopeID string) {
	if s.credPoolAttachments == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "pool attachments are not wired on this deployment")
		return
	}
	a, err := s.credPoolAttachments.Get(r.Context(), scopeID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "no attachment at this level (inherit)")
		return
	}
	s.writeJSONFor(w, r, attachmentViewOf(a))
}

func (s *Server) putPoolAttachment(w http.ResponseWriter, r *http.Request, scopeID, orgID, teamID, auditName string) {
	if s.credPoolAttachments == nil || s.credPoolPools == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "pool attachments are not wired on this deployment")
		return
	}
	id, _ := auth.FromContext(r.Context())
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req poolAttachmentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	if strings.TrimSpace(req.PoolID) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pool_id is required")
		return
	}
	pool, err := s.credPoolPools.Get(r.Context(), req.PoolID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "the attachment names pool %q, which does not exist", req.PoolID)
		return
	}
	if !s.poolAttachmentConsent(r, id, pool, orgID, teamID, scopeID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "the pool's owner (or its audience) must consent — an attachment is an opt-in, never a takeover")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	a := credpool.Attachment{
		ScopeID: scopeID, PoolID: req.PoolID, Enabled: enabled,
		Strategy: credpool.Strategy{Mode: credpool.StrategyMode(req.Mode), MarginPct: req.MarginPct, Triggers: req.Triggers, ResetHold: req.ResetHold},
		Limits:   credpool.DrawLimits{MaxRunsPerDay: req.MaxRunsPerDay, MaxUSDPerDay: req.MaxUSDPerDay},
	}
	if err := a.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%s", err)
		return
	}
	// The trigger vocabulary is validated HERE, at the write — the fold's
	// fail-closed posture backstops, it does not replace: a typo'd trigger
	// stored 200 would brick every later launch of the scope with 422s.
	if err := validatePoolTriggers(a.Strategy.Triggers); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%s", err)
		return
	}
	// A cap-less attachment is allowed: the preview and the launch warn
	// where the draw happens; the save stays quiet (the operator set it
	// deliberately).
	if req.ExpectedUpdatedAt != nil {
		won, err := s.credPoolAttachments.PutIfUnchanged(r.Context(), a, *req.ExpectedUpdatedAt)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "put attachment: %v", err)
			return
		}
		if !won {
			s.httpErrorFor(w, r, http.StatusConflict, "the attachment changed since you read it — reload and re-apply (CAS)")
			return
		}
	} else {
		if err := s.credPoolAttachments.Put(r.Context(), a); err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "put attachment: %v", err)
			return
		}
	}
	s.auditAttachmentWrite(r, scopeID, auditName, a)
	updated, _ := s.credPoolAttachments.Get(r.Context(), scopeID)
	s.writeJSONFor(w, r, attachmentViewOf(updated))
}

func (s *Server) deletePoolAttachment(w http.ResponseWriter, r *http.Request, scopeID, auditName string) {
	if s.credPoolAttachments == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "pool attachments are not wired on this deployment")
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req poolAttachmentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err == nil && req.ExpectedUpdatedAt != nil {
		won, err := s.credPoolAttachments.DeleteIfUnchanged(r.Context(), scopeID, *req.ExpectedUpdatedAt)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusNotFound, "no attachment at this level")
			return
		}
		if !won {
			s.httpErrorFor(w, r, http.StatusConflict, "the attachment changed since you read it — reload and re-apply (CAS)")
			return
		}
	} else {
		if err := s.credPoolAttachments.Delete(r.Context(), scopeID); err != nil {
			s.httpErrorFor(w, r, http.StatusNotFound, "no attachment at this level")
			return
		}
	}
	s.auditAttachmentWrite(r, scopeID, auditName, credpool.Attachment{ScopeID: scopeID})
	s.writeJSONFor(w, r, map[string]string{"status": "detached"})
}

// The per-level wrappers: the level's own write right is the gate, the
// scope id names the doc, the audit name is the trail.

func (s *Server) handleGetPlatformPoolAttachment(w http.ResponseWriter, r *http.Request) {
	s.getPoolAttachment(w, r, credpool.ScopePlatform)
}
func (s *Server) handlePutPlatformPoolAttachment(w http.ResponseWriter, r *http.Request) {
	s.putPoolAttachment(w, r, credpool.ScopePlatform, "", "", "platform.pool_attachment.updated")
}
func (s *Server) handleDeletePlatformPoolAttachment(w http.ResponseWriter, r *http.Request) {
	s.deletePoolAttachment(w, r, credpool.ScopePlatform, "platform.pool_attachment.detached")
}

func (s *Server) handleGetOrgPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this org")
		return
	}
	s.getPoolAttachment(w, r, credpool.OrgScopeID(orgID))
}
func (s *Server) handlePutOrgPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "org admin or owner required")
		return
	}
	s.putPoolAttachment(w, r, credpool.OrgScopeID(orgID), orgID, "", "org.pool_attachment.updated")
}
func (s *Server) handleDeleteOrgPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "org admin or owner required")
		return
	}
	s.deletePoolAttachment(w, r, credpool.OrgScopeID(orgID), "org.pool_attachment.detached")
}

func (s *Server) handleGetTeamPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this team")
		return
	}
	s.getPoolAttachment(w, r, credpool.TeamScopeID(teamID))
}
func (s *Server) handlePutTeamPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "team admin (or org admin) required")
		return
	}
	orgID := s.teamOrgID(r.Context(), teamID)
	s.putPoolAttachment(w, r, credpool.TeamScopeID(teamID), orgID, teamID, "team.pool_attachment.updated")
}
func (s *Server) handleDeleteTeamPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "team admin (or org admin) required")
		return
	}
	s.deletePoolAttachment(w, r, credpool.TeamScopeID(teamID), "team.pool_attachment.detached")
}

func (s *Server) handleGetBotPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this team")
		return
	}
	s.getPoolAttachment(w, r, credpool.BotScopeID(teamID, r.PathValue("bot")))
}
func (s *Server) handlePutBotPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "team admin (or org admin) required")
		return
	}
	orgID := s.teamOrgID(r.Context(), teamID)
	s.putPoolAttachment(w, r, credpool.BotScopeID(teamID, r.PathValue("bot")), orgID, teamID, "bot.pool_attachment.updated")
}
func (s *Server) handleDeleteBotPoolAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "team admin (or org admin) required")
		return
	}
	s.deletePoolAttachment(w, r, credpool.BotScopeID(teamID, r.PathValue("bot")), "bot.pool_attachment.detached")
}

// teamOrgID resolves a team's org for the audience consent read; empty
// when the team is unknown (the consent then answers on owner match
// alone).
func (s *Server) teamOrgID(ctx context.Context, teamID string) string {
	if s.authStore() == nil {
		return ""
	}
	t, err := s.authStore().GetTeam(ctx, teamID)
	if err != nil {
		return ""
	}
	return t.OrgID
}

// handlePoolsSync runs ONE reconciler pass inline — the manual trigger the
// CLI's `pools sync` calls, the onboarding proof (what refreshed, what
// the readings now say).
func (s *Server) handlePoolsSync(w http.ResponseWriter, r *http.Request) {
	if !s.poolSignalSyncStarts() {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "the pool signal reconciler is not wired on this deployment")
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	verdict := s.poolSignalSyncSweep(r.Context())
	switch verdict {
	case lease.VerdictProgressed, lease.VerdictIdle:
		s.writeJSONFor(w, r, map[string]string{"status": "synced", "verdict": verdict.String()})
	default:
		s.httpErrorFor(w, r, http.StatusBadGateway, "the sync pass did not succeed: %s", verdict)
	}
}

// auditPoolWrite / auditAttachmentWrite name the trail. The scope prefix
// rides the pool's owner: a user pool audits under its owner's tenant
// spelling, an org pool under the org, the platform pool under platform.
func (s *Server) auditPoolWrite(r *http.Request, pool credpool.Pool, action string) {
	switch pool.EffectiveOwnerType() {
	case credpool.PoolOwnerOrg:
		s.auditOrg(r, pool.OrgID, "org."+action, "pool", pool.ID, nil)
	case credpool.PoolOwnerPlatform:
		s.auditPlatform(r, "", "platform."+action, "pool", pool.ID, nil)
	default:
		s.auditTenant(r, "user:"+pool.OwnerID, "user."+action, "pool", pool.ID, nil)
	}
}

func (s *Server) auditAttachmentWrite(r *http.Request, scopeID, action string, a credpool.Attachment) {
	switch {
	case scopeID == credpool.ScopePlatform:
		s.auditPlatform(r, "", action, "pool_attachment", a.PoolID, nil)
	case strings.HasPrefix(scopeID, "org:"):
		s.auditOrg(r, strings.TrimPrefix(scopeID, "org:"), action, "pool_attachment", a.PoolID, nil)
	default:
		s.auditTenant(r, scopeID, action, "pool_attachment", a.PoolID, nil)
	}
}
