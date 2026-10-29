package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The tenant-facing CRUD over pkg/connection records (ADR-098, #2759): how a
// team manages its connector connections from the cloud, where the CLI's
// `connections add/list/rm` cannot reach.
//
// Three rules hold HERE rather than by each handler's discipline:
//
//   - EVERY read goes through the Store's tenant-in-the-signature methods
//     with the PATH's team id, so a caller holding another tenant's
//     connection id gets ErrNotFound — the same answer as a record that
//     does not exist, never a distinguishable "forbidden".
//   - NOTHING the route returns carries a credential or anything derived
//     from one: Connection.SealedPayload is json:"-" by construction, and
//     the response DTO below is built field by field so a future field on
//     the record does not silently become part of the API.
//   - The credential is sealed (connection.SealToken, AAD "connection:<id>")
//     before the store ever sees it, on the ONLY paths that accept one —
//     create, and the rotation arm of PATCH.

// connectionDTO is the API shape of one connection. Built field by field —
// see the file comment.
type connectionDTO struct {
	ID            string     `json:"id"`
	Connector     string     `json:"connector"`
	Alias         string     `json:"alias"`
	DisplayName   string     `json:"display_name,omitempty"`
	BaseURL       string     `json:"base_url"`
	SchemeID      string     `json:"scheme_id"`
	Placement     string     `json:"placement"`
	Capabilities  []string   `json:"capabilities"`
	GrantedScopes []string   `json:"granted_scopes,omitempty"`
	ScopesKnown   bool       `json:"scopes_known,omitempty"`
	Status        string     `json:"status"`
	StatusReason  string     `json:"status_reason,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Warnings      []string   `json:"warnings,omitempty"`
}

// connectionDTOOf projects the record. The WARNINGS travel with every read:
// a grant nothing performs (`GrantAdvice`) and a cleartext origin outlive the
// command that wrote them, and the operator reading an inventory months later
// is the one deciding what this connection can be used for.
func connectionDTOOf(c connection.Connection) connectionDTO {
	caps := make([]string, 0, len(c.Capabilities))
	for _, cap := range c.Capabilities {
		caps = append(caps, string(cap))
	}
	d := connectionDTO{
		ID:            c.ID,
		Connector:     c.Connector,
		Alias:         c.Alias,
		DisplayName:   c.DisplayName,
		BaseURL:       c.BaseURL,
		SchemeID:      c.SchemeID,
		Placement:     c.AuthPlacement.Describe(),
		Capabilities:  caps,
		GrantedScopes: c.GrantedScopes,
		ScopesKnown:   c.ScopesKnown,
		Status:        string(c.Status),
		StatusReason:  c.StatusReason,
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
	if !c.ExpiresAt.IsZero() {
		exp := c.ExpiresAt
		d.ExpiresAt = &exp
	}
	if advice := connection.GrantAdvice(c.Capabilities); advice != "" {
		d.Warnings = append(d.Warnings, advice)
	}
	if advice := connection.CleartextOrigin(c.BaseURL); advice != "" {
		d.Warnings = append(d.Warnings, advice)
	}
	return d
}

// connectionCreateReq is the body of POST /api/teams/{id}/connections.
type connectionCreateReq struct {
	Connector string `json:"connector"`
	// Alias is the name a `.bot` writes in `connection:`. Defaults to "main",
	// like the CLI: one connection per connector is the common case.
	Alias        string   `json:"alias"`
	BaseURL      string   `json:"base_url"`
	SchemeID     string   `json:"scheme_id"`
	Capabilities []string `json:"capabilities"`
	DisplayName  string   `json:"display_name"`
	// Token is the credential. Write-only: it is sealed before the store sees
	// it and never read back by any route.
	Token string `json:"token"`
}

// connectionPatchReq is the body of PATCH /api/teams/{id}/connections/{conn_id}.
// Every field is a pointer: absent and "set to empty" are different intents.
type connectionPatchReq struct {
	DisplayName  *string  `json:"display_name,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// Status is the revocation path: "revoked" with a reason an operator can
	// act on. Reactivating ("active") is allowed; "degraded" is the system's
	// word, not an operator's.
	Status       *string `json:"status,omitempty"`
	StatusReason *string `json:"status_reason,omitempty"`
	// Token rotates the credential IN PLACE: a fresh value, sealed onto the
	// same record. Omitted keeps the stored one (keptCredential).
	Token *string `json:"token,omitempty"`
}

func (s *Server) registerConnectionRoutes() {
	s.mux.Handle("GET /api/teams/{id}/connections", s.requireAuth(http.HandlerFunc(s.handleListConnections)))
	s.mux.Handle("POST /api/teams/{id}/connections", s.requireAuth(http.HandlerFunc(s.handleCreateConnection)))
	s.mux.Handle("GET /api/teams/{id}/connections/{conn_id}", s.requireAuth(http.HandlerFunc(s.handleGetConnection)))
	s.mux.Handle("PATCH /api/teams/{id}/connections/{conn_id}", s.requireAuth(http.HandlerFunc(s.handlePatchConnection)))
	s.mux.Handle("DELETE /api/teams/{id}/connections/{conn_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteConnection)))
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	list, err := s.connectionStore.List(r.Context(), teamID, r.URL.Query().Get("connector"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	out := make([]connectionDTO, 0, len(list))
	for _, c := range list {
		out = append(out, connectionDTOOf(c))
	}
	writeJSON(w, struct {
		Connections []connectionDTO `json:"connections"`
	}{Connections: out})
}

func (s *Server) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	c, ok := s.connectionForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	writeJSON(w, connectionDTOOf(c))
}

// connectionForTenant reads one connection of THIS team. A record that exists
// but belongs to another team answers 404 (never 403 — the same
// non-enumeration discipline as the store's tenant filter).
func (s *Server) connectionForTenant(w http.ResponseWriter, r *http.Request, teamID, connID string) (connection.Connection, bool) {
	c, err := s.connectionStore.Get(r.Context(), teamID, connID)
	if err != nil {
		if errors.Is(err, connection.ErrNotFound) {
			httpError(w, http.StatusNotFound, "connection not found")
			return connection.Connection{}, false
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return connection.Connection{}, false
	}
	return c, true
}

func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req connectionCreateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	connectorID := strings.TrimSpace(req.Connector)
	if connectorID == "" {
		httpError(w, http.StatusBadRequest, "connector is required")
		return
	}
	// The credential is validated at the PASTE, like `connections add` and
	// `secret set`: a value that could not possibly authenticate is refused
	// here, not discovered as a provider 401 in the middle of a run.
	if err := secrets.ValidateTokenShape("token", req.Token); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	alias := strings.TrimSpace(req.Alias)
	if alias == "" {
		alias = "main"
	}

	// The package is resolved FIRST, so an unknown connector or scheme is
	// refused before a credential is sealed anywhere.
	pkg, tier, err := s.connectorCatalog.Package(connectorID)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	schemeID, err := resolveConnectionScheme(pkg, req.SchemeID)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	scheme, _ := pkg.Connector.AuthScheme(schemeID)
	if scheme.Kind == spec.AuthBasic {
		httpError(w, http.StatusBadRequest,
			"scheme %q of connector %q is HTTP basic, which needs a username and a password — this route seals a token only; name a token-shaped scheme in scheme_id", schemeID, pkg.Connector.ID)
		return
	}
	baseURL, err := resolveConnectionBaseURL(pkg, req.BaseURL)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	caps, err := parseConnectionCapabilities(req.Capabilities)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}

	connID, err := store.GenerateRunID()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "generate id: %s", err.Error())
		return
	}
	connID = "conn_" + connID
	sealed, err := connection.SealToken(s.sealer, connID, req.Token, time.Time{})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}

	conn := connection.Connection{
		ID: connID, TenantID: teamID,
		Connector: connectorID, Alias: alias,
		DisplayName: strings.TrimSpace(req.DisplayName), BaseURL: baseURL,
		SchemeID: schemeID, AuthPlacement: connection.PlacementOf(scheme),
		Capabilities: caps, Status: connection.StatusActive, SealedPayload: sealed,
		// Deliberately NOT claiming to know the grant: a provider that never
		// enumerated a PAT's scopes has told us nothing (the CLI writes the
		// same third state).
		ScopesKnown: false,
	}
	if err := s.connectionStore.Create(r.Context(), conn); err != nil {
		if errors.Is(err, connection.ErrExists) {
			httpError(w, http.StatusConflict, "a %q connection named %q already exists — revoke or rename it first", connectorID, alias)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "connection.created", "connection", connID, map[string]any{
		"connector": connectorID, "alias": alias, "tier": tier.String(),
	})
	writeJSON(w, connectionDTOOf(conn))
}

func (s *Server) handlePatchConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	conn, ok := s.connectionForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	var req connectionPatchReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DisplayName != nil {
		conn.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Capabilities != nil {
		caps, err := parseConnectionCapabilities(req.Capabilities)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		conn.Capabilities = caps
	}
	if req.Status != nil {
		switch connection.Status(strings.TrimSpace(*req.Status)) {
		case connection.StatusActive:
			conn.Status = connection.StatusActive
			conn.StatusReason = ""
		case connection.StatusRevoked:
			conn.Status = connection.StatusRevoked
			if req.StatusReason != nil {
				conn.StatusReason = strings.TrimSpace(*req.StatusReason)
			}
		default:
			httpError(w, http.StatusBadRequest,
				"status must be %q or %q — %q is the system's word, set when a credential degrades, not an operator's",
				connection.StatusActive, connection.StatusRevoked, connection.StatusDegraded)
			return
		}
	}
	if req.Token != nil {
		// Rotation: the same shape gate as creation, applied at the paste.
		if err := secrets.ValidateTokenShape("token", *req.Token); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		sealed, err := connection.SealToken(s.sealer, conn.ID, *req.Token, time.Time{})
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%s", err.Error())
			return
		}
		conn.SealedPayload = sealed
	}
	if err := s.connectionStore.Update(r.Context(), conn); err != nil {
		if errors.Is(err, connection.ErrExists) {
			httpError(w, http.StatusConflict, "%s", err.Error())
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "connection.updated", "connection", conn.ID, map[string]any{
		"connector": conn.Connector, "alias": conn.Alias, "status": string(conn.Status),
	})
	writeJSON(w, connectionDTOOf(conn))
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	connID := r.PathValue("conn_id")
	if err := s.connectionStore.Delete(r.Context(), teamID, connID); err != nil {
		if errors.Is(err, connection.ErrNotFound) {
			httpError(w, http.StatusNotFound, "connection not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "connection.deleted", "connection", connID, nil)
	w.WriteHeader(http.StatusNoContent)
}

// resolveConnectionScheme picks the auth scheme, refusing to guess when the
// package declares several: placement differs between them, so a wrong guess
// is a 401 that reads like a bad credential.
func resolveConnectionScheme(pkg *spec.Package, want string) (string, error) {
	ids := make([]string, 0, len(pkg.Connector.Auth))
	for _, a := range pkg.Connector.Auth {
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	want = strings.TrimSpace(want)
	if want != "" {
		for _, id := range ids {
			if id == want {
				return id, nil
			}
		}
		return "", fmt.Errorf("connector %q has no auth scheme %q (it declares: %s)", pkg.Connector.ID, want, strings.Join(ids, ", "))
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("connector %q declares no auth scheme", pkg.Connector.ID)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("connector %q declares several auth schemes (%s) — name one in scheme_id", pkg.Connector.ID, strings.Join(ids, ", "))
	}
}

// resolveConnectionBaseURL pins the instance URL the credential may be sent
// to: the request's, else the package's default, else refused. What a
// credential may be sent to is decided when it is entrusted, never re-derived
// at call time.
func resolveConnectionBaseURL(pkg *spec.Package, baseURL string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(pkg.Connector.BaseURL.Default, "/")
	}
	if baseURL == "" {
		return "", fmt.Errorf("connector %q names no default instance, so base_url is required (e.g. https://git.example.com)", pkg.Connector.ID)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("base_url %q does not parse: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base_url %q needs an http or https scheme (e.g. https://%s)", baseURL, strings.TrimPrefix(baseURL, "//"))
	}
	if u.Host == "" {
		return "", fmt.Errorf("base_url %q names no host", baseURL)
	}
	return baseURL, nil
}

// parseConnectionCapabilities turns the request's capability names into the
// record's, defaulting to `action` alone — the narrower grant, since an agent
// choosing its own calls is a decision an operator makes deliberately.
func parseConnectionCapabilities(vals []string) ([]connection.Capability, error) {
	if len(vals) == 0 {
		return []connection.Capability{connection.CapAction}, nil
	}
	out := make([]connection.Capability, 0, len(vals))
	for _, v := range vals {
		c := connection.Capability(strings.TrimSpace(v))
		if !connection.ValidCapability(c) {
			return nil, fmt.Errorf("unknown capability %q (want one or more of %s)", v, strings.Join(connection.CapabilityNames(), ", "))
		}
		out = append(out, c)
	}
	return out, nil
}
