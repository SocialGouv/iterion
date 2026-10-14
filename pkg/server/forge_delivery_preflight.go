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
	// A verdict is an answer ABOUT A CREDENTIAL, and it is the only shape a
	// caller may publish as a blocking check on a pull request. This
	// service's own store, sealer, configuration or connection record failing
	// establishes nothing about anybody's token, so those paths leave with a
	// status and never with a verdict: a caller reads a non-200 as "the
	// preflight did not answer" and ends on a relaunch instead of on
	// somebody's permission. Exactly ONE site below refuses — the permission
	// map the provider recorded when it minted the very token this run
	// mounted — and every other term of every record leaves with a status;
	// TestForgeDeliveryPreflightRefusesOnlyFromTheProvidersAnswer pins that
	// count and sweeps every field of the records read here against a granting
	// provider map — each field moved to every value its own type declares, to
	// every string value this file spells, and to the absent and the
	// unexpected: a second refusal site here is a statement about somebody's
	// credential that nobody answered, published as a blocking check on a
	// third party's pull request.
	deny := func(reason string) {
		writeJSON(w, forgeDeliveryVerdict{Code: "FORGE_PERMISSION_DENIED", Reason: reason})
	}
	if s.genericSecrets == nil || s.sealer == nil {
		httpError(w, http.StatusServiceUnavailable, "the managed credential store is unavailable, so no delivery permission can be read")
		return
	}
	// The connection RECORD is this service's bookkeeping too. Which kind of
	// credential a connection holds says what this endpoint can read, not what
	// a forge granted; and the record reading active says only that the last
	// refresh of the connection's own admin credential worked — one 401 there
	// flips it to StatusNeedsReauth (forge.RefreshWorker.markRevoked) or, on a
	// permission mismatch, to StatusDegraded (markDegraded), while the
	// installation token the run mounted stays live, with its mint proof, for
	// the rest of its hour.
	if conn.Provider != forge.ProviderGitHub || conn.Kind != forge.KindGitHubApp {
		httpError(w, http.StatusConflict, "this connection's runtime credential is not a managed GitHub App token, so its delivery permission cannot be read here")
		return
	}
	if conn.Status != forge.StatusActive || conn.ManagedSecretID == "" {
		httpError(w, http.StatusServiceUnavailable, "this connection holds no active managed credential, so its delivery permission is unknown")
		return
	}
	rec, err := s.genericSecrets.Get(ctx, conn.ManagedSecretID)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "the managed runtime credential could not be read")
		return
	}
	if rec.TenantID != grant.TeamID {
		httpError(w, http.StatusForbidden, "the managed runtime credential is outside the run grant")
		return
	}
	token, err := secrets.OpenGenericSecret(s.sealer, rec.ID, rec.SealedSecret)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "the managed runtime credential could not be opened")
		return
	}
	defer clear(token)
	// A verdict needs a live mint proof OF THE TOKEN THIS RUN MOUNTED, and
	// holding one is this service's own bookkeeping rather than a fact about
	// anybody's grant: iterion rewrites the managed credential's plaintext in
	// place at every point of use (forge.Orchestrator.EnsureManagedSecret ->
	// narrowGitHubAppSecret, best-effort and with no freshness guard) and five
	// minutes before the installation token's hour runs out
	// (forge.RefreshWorker), while a run re-reads its mounted file every
	// runner.fileSecretRefreshInterval -- so a run legitimately holds the
	// previous live token of its own App for minutes at a time. Of the four
	// terms of secrets.TokenPermissionProof.Allows only the permission map the
	// provider answered at mint time is a fact about the grant; the other
	// three leave with a status, so the caller relaunches instead of
	// publishing a hold about a credential nothing was established about.
	now := time.Now()
	if secrets.TokenSHA256(string(token)) != strings.ToLower(req.CredentialSHA256) {
		httpError(w, http.StatusConflict, "the token mounted in this run is not the credential currently stored for this connection, so its delivery permission is unknown")
		return
	}
	proof := rec.ForgeTokenProof
	if proof == nil || proof.SHA256 != secrets.TokenSHA256(string(token)) || !now.Before(proof.ExpiresAt) {
		httpError(w, http.StatusServiceUnavailable, "no live mint proof for the credential this connection stores, so its delivery permission is unknown")
		return
	}
	for _, permission := range []string{"workflows", "contents"} {
		if !proof.Allows(string(token), permission, now) {
			deny(permission + ":write is missing or unverified on the runtime token; an operator must approve the App's delivery permissions, refresh its token and relaunch")
			return
		}
	}
	writeJSON(w, forgeDeliveryVerdict{OK: true, Reason: "runtime token has confirmed workflows:write and contents:write"})
}
