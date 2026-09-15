package server

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

type forgeDeliveryVerdict struct {
	OK     bool   `json:"ok"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// This read-only capability check authenticates the same scoped run grant as
// forge publishing. It never mints a broader token or tries a write operation.
func (s *Server) handleForgeDeliveryPreflight(w http.ResponseWriter, r *http.Request) {
	if s.forgePublishTokens == nil || s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge delivery preflight is unavailable")
		return
	}
	grant, ok := s.forgePublishTokens.lookup(r.Header.Get("X-Iterion-Run"))
	if !ok {
		httpError(w, http.StatusUnauthorized, "unknown or expired run token")
		return
	}
	var req struct {
		PRURL            string `json:"pr_url"`
		CredentialSHA256 string `json:"credential_sha256"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid preflight request")
		return
	}
	digest, err := hex.DecodeString(req.CredentialSHA256)
	if err != nil || len(digest) != 32 {
		httpError(w, http.StatusBadRequest, "credential_sha256 must be a full SHA256 digest")
		return
	}
	host, repo, _, err := forge.ParsePullURL(req.PRURL)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid pr_url")
		return
	}
	if !strings.EqualFold(repo, grant.Repo) {
		httpError(w, http.StatusForbidden, "run token is scoped to another repository")
		return
	}
	ctx := store.WithTenant(r.Context(), grant.TeamID)
	conn, err := s.forgeConnections.Get(ctx, grant.ConnectionID)
	if err != nil || conn.TenantID != grant.TeamID {
		httpError(w, http.StatusNotFound, "connection not found")
		return
	}
	if !strings.EqualFold(hostOfURL(conn.BaseURL()), host) {
		httpError(w, http.StatusForbidden, "pr_url host is outside the run grant")
		return
	}
	deny := func(reason string) {
		writeJSON(w, forgeDeliveryVerdict{Code: "FORGE_PERMISSION_DENIED", Reason: reason})
	}
	if conn.Provider != forge.ProviderGitHub || conn.Kind != forge.KindGitHubApp || conn.Status != forge.StatusActive || conn.ManagedSecretID == "" || s.genericSecrets == nil || s.sealer == nil {
		deny("workflows:write cannot be verified for this runtime credential; use a managed GitHub App token with confirmed delivery permissions")
		return
	}
	rec, err := s.genericSecrets.Get(ctx, conn.ManagedSecretID)
	if err != nil || rec.TenantID != grant.TeamID {
		deny("workflows:write cannot be verified: the managed runtime credential is unavailable")
		return
	}
	token, err := secrets.OpenGenericSecret(s.sealer, rec.ID, rec.SealedSecret)
	if err != nil {
		deny("workflows:write cannot be verified: the managed runtime credential cannot be opened")
		return
	}
	defer clear(token)
	if secrets.TokenSHA256(string(token)) != strings.ToLower(req.CredentialSHA256) {
		deny("workflows:write cannot be verified for the token mounted in this run; refresh the run credential and relaunch")
		return
	}
	for _, permission := range []string{"workflows", "contents"} {
		if !rec.ForgeTokenProof.Allows(string(token), permission, time.Now()) {
			deny(permission + ":write is missing or unverified on the runtime token; an operator must approve the App's delivery permissions, refresh its token and relaunch")
			return
		}
	}
	writeJSON(w, forgeDeliveryVerdict{OK: true, Reason: "runtime token has confirmed workflows:write and contents:write"})
}
