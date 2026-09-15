package server

import (
	"context"
	"slices"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// identifyOAuthAccount enriches a validated Claude credential. An unavailable
// profile never removes a working credential, and a label is never identity.
func (s *Server) identifyOAuthAccount(ctx context.Context, rec *secrets.OAuthRecord, payload []byte) {
	if rec.Kind != secrets.OAuthKindClaudeCode {
		return
	}
	v, err := secrets.ParseAnthropicView(payload)
	if err != nil {
		return // the ingestion shape gate already rejected this case
	}
	if !slices.Contains(rec.Scopes, "user:profile") {
		rec.AccountError = "Profile unavailable: this credential has no user:profile scope; reconnect through the browser to identify the account."
	} else {
		a, err := secrets.DiscoverAnthropicAccount(ctx, s.httpClient, v.ClaudeAIOauth.AccessToken)
		if err == nil {
			now := time.Now().UTC()
			rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail = a.ID, a.OrganizationID, a.Email
			rec.AccountCheckedAt = &now
			rec.Fingerprint = a.Fingerprint()
			return
		}
		rec.AccountError = err.Error()
	}
	// A failed lookup may retain a previously verified identity ONLY for the
	// exact same bearer token. The same rank or human label proves nothing.
	prev, err := s.resolveOAuthRecord(ctx, rec.UserID, rec.Kind, rec.Rank)
	if err != nil || !secrets.IsAccountFingerprint(prev.Fingerprint) {
		return
	}
	old, err := secrets.OpenOAuthPayload(s.sealer, prev.UserID, prev.Kind, prev.SealedPayload)
	if err != nil {
		return
	}
	oldView, err := secrets.ParseAnthropicView(old)
	if err != nil || oldView.ClaudeAIOauth.AccessToken != v.ClaudeAIOauth.AccessToken {
		return
	}
	rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail = prev.AccountID, prev.AccountOrganizationID, prev.AccountEmail
	rec.AccountCheckedAt = prev.AccountCheckedAt
	rec.Fingerprint = prev.Fingerprint
}
