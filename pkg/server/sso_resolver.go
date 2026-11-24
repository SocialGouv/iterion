package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// outboundStrict reports whether server-side fetches of an operator/admin-
// supplied URL (OIDC issuer discovery/token/userinfo, the studio preview proxy)
// must enforce the public-unicast SSRF guard: always in cloud mode, and in
// local mode when bound to a non-loopback address (multi-user dev box / LAN
// exposure). Shared by the preview proxy and the per-org SSO connectors.
func (s *Server) outboundStrict() bool {
	return s.cfg.Mode == "cloud" || !httpdial.IsLoopbackBind(s.cfg.Bind)
}

// buildOrgOIDCConnector assembles a per-org GenericConnector from a stored row:
// unseal the client secret, derive SSRF strictness, attach the matching
// SafeClient. The single source of truth for "what a per-org OIDC connector
// looks like", shared by resolveConnector (login flows) and the config test
// endpoint — the caller owns the enabled/kind gating.
func (s *Server) buildOrgOIDCConnector(row orgsso.OrgSSOProvider) (oidc.Connector, error) {
	secret, err := orgsso.OpenClientSecret(s.sealer, row.ID, row.SealedSecret)
	if err != nil {
		return nil, err
	}
	strict := s.outboundStrict()
	return oidc.NewGenericConnectorWithSlug(
		row.OIDCSlug(), row.IssuerURL, row.ClientID, secret, row.DisplayName, row.Scopes,
		oidc.SafeGenericClient(strict), strict,
	), nil
}

// resolveConnector returns the OIDC connector for a provider slug. Global
// connectors (github, google, the deployment-wide "sso") come from the static
// registry; a per-org slug ("oidc-org-<id>") is built on demand from the
// tenant's stored provider row, with an SSRF-guarded HTTP client.
//
// tenantID and providerID are non-empty only for per-org connectors. The
// callback drives the per-org login from the values it persisted in
// PendingAuth at /start — never from these resolver outputs or the URL — so a
// slug presented at /callback can never be coerced into another tenant's
// policy (provider/tenant confusion is closed).
func (s *Server) resolveConnector(ctx context.Context, slug string) (oidc.Connector, string, string, error) {
	if s.oidcRegistry != nil {
		if c, err := s.oidcRegistry.Get(slug); err == nil {
			return c, "", "", nil
		}
	}
	id, ok := orgsso.ParseOIDCSlug(slug)
	if !ok || s.orgSSO == nil || s.sealer == nil {
		return nil, "", "", oidc.ErrUnknownProvider
	}
	row, err := s.orgSSO.Get(ctx, id)
	// A read that FAILED is not the fact "no such provider": only the
	// store's own ErrNotFound says the row is absent. Collapsing the two
	// answered every unreachable store with a 404 at /start and an
	// "unknown_provider" banner at /callback, with nothing logged — the
	// tenant's whole SSO looked deleted for as long as the store was down.
	if err != nil && !errors.Is(err, orgsso.ErrNotFound) {
		return nil, "", "", fmt.Errorf("%w: read the SSO provider %s: %w", oidc.ErrProviderUnavailable, id, err)
	}
	if err != nil || row.Kind != orgsso.KindOIDC {
		return nil, "", "", oidc.ErrUnknownProvider
	}
	// A configured-but-disabled provider is distinct from a non-existent one:
	// the callback maps ErrProviderDisabled to a "disabled" SPA banner so the
	// user isn't told the provider is "unknown" when the admin merely toggled
	// it off.
	if !row.Enabled {
		return nil, "", "", oidc.ErrProviderDisabled
	}
	conn, err := s.buildOrgOIDCConnector(row)
	if err != nil {
		// The row is there and enabled: a secret this deployment's sealer
		// cannot open is ITS failure, which the operator has to see.
		return nil, "", "", fmt.Errorf("%w: build the OIDC connector of provider %s: %w", oidc.ErrProviderUnavailable, id, err)
	}
	return conn, row.TenantID, row.ID, nil
}

// refuseConnector answers a JSON route whose resolveConnector failed, and is
// the ONE place that classification lives for them: a resolution that FAILED
// is not the fact that the provider is absent, so oidc.ErrProviderUnavailable
// owes the identity contract — 5xx, generic body, the cause in the log —
// while absent-or-not-OIDC keeps the word the route gives its client (status,
// msg). A consumer that collapses the two tells a caller the SSO they are
// configured for does not exist, and logs nothing for the operator.
//
// The browser callback does NOT use it: its answer is a banner redirect and
// it splits a third case (ErrProviderDisabled) the API routes have no word
// for — see handleOIDCCallback.
func (s *Server) refuseConnector(w http.ResponseWriter, r *http.Request, name string, err error, status int, msg string) {
	if errors.Is(err, oidc.ErrProviderUnavailable) {
		s.identityFailure(w, r, fmt.Errorf("resolve the SSO provider %q: %w", name, err))
		return
	}
	httpError(w, status, "%s", msg)
}
