package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
)

// oidcAgentBindingCookie is the per-flow HttpOnly cookie set at
// /api/auth/oidc/<provider>/start and verified at the matching
// /callback. RFC 9700 (OAuth 2.0 Security BCP) §4.7.1: the `state`
// parameter proves freshness/uniqueness but does NOT bind the flow to
// the user agent. Without this cookie an attacker who completes
// /start in their browser and lures the victim into the resulting
// callback pins the victim into the attacker's account on iterion
// (classic login-CSRF / session fixation against OAuth/OIDC).
//
// Path-scoped to /api/auth/oidc/ so unrelated requests don't carry
// the value. 10 min MaxAge matches the StateStore TTL.
const oidcAgentBindingCookie = "iterion_oidc_agent"

// SSO callback error codes. The OIDC callback is a top-level browser
// navigation, so failures must NOT render as a bare API error page —
// instead we redirect to the SPA login route with one of these stable
// codes in `?sso_error=`, and the SPA maps it to a friendly message +
// the right next step. The raw provider detail stays in the server log
// (handleOIDCCallback) and is never reflected to the browser.
const (
	ssoErrUnknownProvider  = "unknown_provider"
	ssoErrProviderDisabled = "disabled"
	ssoErrProviderReturned = "provider_error"
	ssoErrStateExpired     = "state_expired"
	ssoErrAgentBinding     = "agent_binding"
	ssoErrExchangeFailed   = "exchange_failed"
	ssoErrLinkRequired     = "link_required"
	ssoErrRestricted       = "restricted"
	ssoErrLoginFailed      = "login_failed"
)

// redirectSSOError aborts an OIDC callback by redirecting the browser to the
// SPA login screen with a stable `?sso_error=<code>` (plus optional context
// like the provider display name), so the SPA can render a clean banner
// instead of the user landing on a raw `400 Bad Request` API page. The target
// is always a server-built relative `/login` path — never anything derived
// from a user-supplied `next` — so this can't become an open redirect.
func redirectSSOError(w http.ResponseWriter, r *http.Request, code string, extra url.Values) {
	q := url.Values{}
	q.Set("sso_error", code)
	for k, vs := range extra {
		for _, v := range vs {
			if v != "" {
				q.Add(k, v)
			}
		}
	}
	http.Redirect(w, r, "/login?"+q.Encode(), http.StatusFound)
}

// ssoErrorForAuth maps an auth-service login error to its SPA error code.
func ssoErrorForAuth(err error) string {
	switch {
	case errors.Is(err, auth.ErrLinkRequiresConsent):
		return ssoErrLinkRequired
	case errors.Is(err, auth.ErrSSORestricted):
		return ssoErrRestricted
	default:
		return ssoErrLoginFailed
	}
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	type provider struct {
		Name    string `json:"name"`
		Display string `json:"display"`
	}
	out := struct {
		SignupMode string     `json:"signup_mode"`
		Providers  []provider `json:"providers"`
	}{SignupMode: s.cfg.SignupMode}
	seen := make(map[string]struct{})
	add := func(name, display string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out.Providers = append(out.Providers, provider{Name: name, Display: display})
	}
	if s.oidcRegistry != nil {
		for _, c := range s.oidcRegistry.Enabled() {
			add(c.Name(), c.Display())
		}
	}
	// Per-org providers (a tenant's own Keycloak). The org is resolved EITHER
	// from an explicit slug (?org=) OR — the friendlier default — from the
	// user's email/domain (?email= / ?domain=) via the org's verified domains,
	// so a user never has to know their org's slug. Both paths return only the
	// global providers for an unknown org — never 404 — so this anonymous
	// endpoint is not an org-existence oracle.
	if s.orgSSO != nil {
		for _, tenantID := range s.resolveOrgTenants(r) {
			rows, _ := s.orgSSO.ListByTenantKind(r.Context(), tenantID, orgsso.KindOIDC)
			for _, row := range rows {
				if !row.Enabled {
					continue
				}
				disp := row.DisplayName
				if disp == "" {
					disp = "SSO"
				}
				add(row.OIDCSlug(), disp)
			}
		}
	}
	writeJSON(w, out)
}

// resolveOrgTenants returns the tenant ids whose per-org SSO should be offered
// on the login screen, resolved from the request's ?org= slug and/or
// ?email=/?domain= (matched against verified domains). Best-effort and
// non-oracle: any miss yields no tenant rather than an error.
func (s *Server) resolveOrgTenants(r *http.Request) []string {
	var out []string
	seen := make(map[string]struct{})
	push := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	if org := strings.TrimSpace(r.URL.Query().Get("org")); org != "" {
		// SSO is org-level, but stored under the org's primary team
		// (the storage tenant) — resolve the public org slug to that team.
		if o, err := s.authStore().GetOrgBySlug(r.Context(), org); err == nil {
			push(s.firstTeamInOrg(r.Context(), o.ID))
		}
	}
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		domain = orgsso.EmailDomain(r.URL.Query().Get("email"))
	}
	if domain != "" && s.orgDomains != nil {
		if tenants, err := s.orgDomains.TenantsForDomain(r.Context(), domain); err == nil {
			for _, id := range tenants {
				push(id)
			}
		}
	}
	return out
}

// ---- OIDC handlers ----

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	s.beginOIDCFlow(w, r, r.PathValue("provider"), "")
}

// handleOIDCLinkStart begins an SSO flow that, on completion, attaches the
// resolved identity to the already-authenticated caller (the exit from the 409
// "link requires consent" dead-end). It lives under /api/me/ — NOT the public
// /api/auth/oidc/ namespace — so requireAuth runs and the caller is known. The
// shared callback distinguishes the two via PendingAuth.LinkUserID.
func (s *Server) handleOIDCLinkStart(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || id.UserID == "" {
		httpError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	s.beginOIDCFlow(w, r, r.PathValue("provider"), id.UserID)
}

// beginOIDCFlow resolves the connector, persists PendingAuth and redirects to
// the IdP authorize URL. linkUserID is empty for a sign-in flow and set to the
// caller's user id for the connect-from-settings flow.
func (s *Server) beginOIDCFlow(w http.ResponseWriter, r *http.Request, name, linkUserID string) {
	c, tenantID, providerID, err := s.resolveConnector(r.Context(), name)
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown provider")
		return
	}
	state, verifier, _, err := oidc.GenerateStateAndPKCE()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	redirectURI := s.oidcRedirectURI(name)
	// A link flow always returns to settings; only a sign-in honours ?next=.
	// A DESKTOP sign-in instead validates ?next= as a loopback URL and, at the
	// callback, mints a single-use ticket + 302s there (see handleOIDCCallback
	// + desktop_sso.go) rather than setting browser cookies.
	next := ""
	desktop := false
	desktopRedirect := ""
	if linkUserID == "" {
		if r.URL.Query().Get("desktop") == "1" {
			desktopRedirect = safeDesktopRedirect(r.URL.Query().Get("next"))
			if desktopRedirect == "" {
				httpError(w, http.StatusBadRequest, "desktop flow requires a loopback next= URL")
				return
			}
			desktop = true
		} else {
			next = safeNext(r.URL.Query().Get("next"))
		}
	}
	authURL, err := c.AuthorizeURL(r.Context(), redirectURI, state, verifier)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "build authorize URL: %v", err)
		return
	}
	// The agent-binding cookie is a BROWSER login-CSRF guard: it is set on the
	// /start response and re-presented by the same browser at /callback. A
	// DESKTOP flow calls /start from its native client (so the cookie would go
	// there, not the browser that completes the callback) — binding is instead
	// guaranteed by the desktop-controlled loopback listener + the single-use
	// state + PKCE. So skip the cookie and store an empty AgentBinding, which
	// the callback treats as "no browser binding to check" (same as CLI/SDK).
	binding := ""
	if !desktop {
		binding, err = newAgentBindingToken()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err := s.oidcStates.Put(r.Context(), oidc.PendingAuth{
		Provider:        name,
		State:           state,
		CodeVerifier:    verifier,
		RedirectURI:     redirectURI,
		NextURL:         next,
		IssuedAt:        time.Now().UTC(),
		AgentBinding:    binding,
		TenantID:        tenantID,
		OrgProviderID:   providerID,
		LinkUserID:      linkUserID,
		Desktop:         desktop,
		DesktopRedirect: desktopRedirect,
	}); err != nil {
		httpError(w, http.StatusInternalServerError, "persist state: %v", err)
		return
	}
	if binding != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     oidcAgentBindingCookie,
			Value:    binding,
			Path:     "/api/auth/oidc/",
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			// SameSite=Lax is required: the callback is a top-level GET
			// navigation from the IdP and Strict would block the cookie.
			// Lax is sufficient because we additionally require the cookie
			// value to match PendingAuth.AgentBinding at /callback — a
			// cross-site script can't read the cookie (HttpOnly) and can't
			// set a cookie for iterion's origin (same-origin policy).
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int((10 * time.Minute).Seconds()),
		})
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, map[string]string{"authorize_url": authURL})
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// newAgentBindingToken returns a 32-byte base64url-encoded random
// token used as the OIDC flow's user-agent binding cookie.
func newAgentBindingToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// clearOIDCAgentBindingCookie deletes the per-flow agent-binding
// cookie. Called at /callback (regardless of outcome) so each cookie
// is used at most once.
func clearOIDCAgentBindingCookie(w http.ResponseWriter, domain string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcAgentBindingCookie,
		Value:    "",
		Path:     "/api/auth/oidc/",
		Domain:   domain,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	c, _, _, err := s.resolveConnector(r.Context(), name)
	if err != nil {
		// The callback is a top-level navigation, so surface failures as a
		// friendly SPA banner rather than a bare API error page.
		code := ssoErrUnknownProvider
		if errors.Is(err, oidc.ErrProviderDisabled) {
			code = ssoErrProviderDisabled
		}
		redirectSSOError(w, r, code, nil)
		return
	}
	provQ := url.Values{"sso_provider": {c.Display()}}
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		// Don't reflect the provider's error_description verbatim:
		// some providers include server-side context (account ids,
		// internal flags) that we shouldn't surface to the SPA. The
		// short OAuth error code is sufficient for the UI; full
		// detail (if needed) is in the server log.
		if s.logger != nil {
			s.logger.Warn("oidc callback error from %s: code=%s description=%q",
				name, oauthErr, r.URL.Query().Get("error_description"))
		}
		redirectSSOError(w, r, ssoErrProviderReturned, provQ)
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		redirectSSOError(w, r, ssoErrStateExpired, provQ)
		return
	}
	pending, err := s.oidcStates.Take(r.Context(), state)
	if err != nil {
		redirectSSOError(w, r, ssoErrStateExpired, provQ)
		return
	}
	if pending.Provider != name {
		redirectSSOError(w, r, ssoErrStateExpired, provQ)
		return
	}
	// Verify the user-agent binding cookie matches the one issued at
	// /start (login-CSRF guard per RFC 9700 §4.7.1). Constant-time
	// compare avoids timing leaks on near-miss values. The cookie is
	// cleared regardless of outcome — single-use semantics.
	if pending.AgentBinding != "" {
		ck, cerr := r.Cookie(oidcAgentBindingCookie)
		clearOIDCAgentBindingCookie(w, s.cfg.CookieDomain, s.cfg.CookieSecure)
		if cerr != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(pending.AgentBinding)) != 1 {
			redirectSSOError(w, r, ssoErrAgentBinding, provQ)
			return
		}
	}
	ext, err := c.ExchangeCode(r.Context(), code, pending.RedirectURI, pending.CodeVerifier)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("oidc exchange failed for %s: %v", name, err)
		}
		redirectSSOError(w, r, ssoErrExchangeFailed, provQ)
		return
	}
	// Link flow: an authenticated user is attaching this SSO identity to their
	// existing account (started from /link/start). Attach and bounce back to
	// settings instead of running login/signup.
	if pending.LinkUserID != "" {
		if err := s.authSvc.LinkExternalToUser(r.Context(), ext, pending.LinkUserID); err != nil {
			if s.logger != nil {
				s.logger.Warn("oidc link failed for user %s via %s: %v", pending.LinkUserID, name, err)
			}
			http.Redirect(w, r, "/settings?sso_link_error="+url.QueryEscape(linkErrorCode(err)), http.StatusFound)
			return
		}
		http.Redirect(w, r, "/settings?sso_linked="+url.QueryEscape(c.Display()), http.StatusFound)
		return
	}
	// Per-org flows drive the login from the tenant/provider stored in
	// PendingAuth at /start (server-side, keyed by the IdP-echoed state) — NOT
	// from the URL — so a slug presented at /callback cannot be coerced into
	// another tenant's policy.
	var res auth.LoginResult
	if pending.TenantID != "" {
		res, err = s.authSvc.LoginWithExternalForOrg(r.Context(), ext, pending.TenantID, pending.OrgProviderID, r.UserAgent(), s.clientIP(r))
	} else {
		res, err = s.authSvc.LoginWithExternal(r.Context(), ext, r.UserAgent(), s.clientIP(r))
	}
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("sso login failed via %s: %v", name, err)
		}
		redirectSSOError(w, r, ssoErrorForAuth(err), provQ)
		return
	}
	// Desktop flow: don't set browser cookies. Mint a single-use ticket holding
	// this LoginResult and 302 to the desktop's loopback listener with it; the
	// desktop redeems it at /api/auth/desktop/exchange over its native client
	// (see desktop_sso.go). The refresh token thus never enters a URL.
	if pending.Desktop {
		if s.desktopTickets == nil {
			redirectSSOError(w, r, ssoErrExchangeFailed, provQ)
			return
		}
		ticket, mintErr := s.desktopTickets.Mint(r.Context(), res)
		if mintErr != nil {
			redirectSSOError(w, r, ssoErrExchangeFailed, provQ)
			return
		}
		sep := "?"
		if strings.Contains(pending.DesktopRedirect, "?") {
			sep = "&"
		}
		http.Redirect(w, r, pending.DesktopRedirect+sep+"ticket="+url.QueryEscape(ticket), http.StatusFound)
		return
	}
	s.setAuthCookies(w, res.AccessToken, res.AccessExpires, res.RefreshToken, res.RefreshExpires)
	target := pending.NextURL
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// linkErrorCode maps a link-flow error to the SPA settings banner code.
func linkErrorCode(err error) string {
	if errors.Is(err, auth.ErrLinkAlreadyOwned) {
		return "already_linked"
	}
	return "failed"
}

// ssoLinkView is the public shape of a connected SSO identity.
type ssoLinkView struct {
	Provider       string `json:"provider"`
	ProviderUserID string `json:"provider_user_id"`
	Email          string `json:"email,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
}

// handleListMySSOLinks returns the caller's connected SSO identities.
func (s *Server) handleListMySSOLinks(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	links, err := s.authSvc.ListSSOLinks(r.Context(), id.UserID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	out := struct {
		Links []ssoLinkView `json:"links"`
	}{Links: make([]ssoLinkView, 0, len(links))}
	for _, l := range links {
		v := ssoLinkView{Provider: l.Provider, ProviderUserID: l.ProviderUserID, Email: l.Email}
		if !l.CreatedAt.IsZero() {
			v.CreatedAt = l.CreatedAt.UTC().Format(time.RFC3339)
		}
		out.Links = append(out.Links, v)
	}
	writeJSON(w, out)
}

// handleUnlinkMySSO disconnects one of the caller's SSO identities.
func (s *Server) handleUnlinkMySSO(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	provider := r.PathValue("provider")
	subject := r.PathValue("subject")
	if provider == "" || subject == "" {
		httpError(w, http.StatusBadRequest, "provider and subject required")
		return
	}
	if err := s.authSvc.UnlinkExternal(r.Context(), id.UserID, provider, subject); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) oidcRedirectURI(provider string) string {
	base := s.cfg.PublicURL
	if base == "" {
		// Local fallback: use the bound address.
		base = fmt.Sprintf("http://%s:%d", s.cfg.Bind, s.cfg.Port)
	}
	return base + "/api/auth/oidc/" + provider + "/callback"
}

// safeNext sanitizes the post-login redirect target to avoid open
// redirects: only same-origin, relative paths starting with "/" and
// not "//" are allowed.
func safeNext(v string) string {
	if v == "" {
		return ""
	}
	// Reject backslashes outright: WHATWG-compliant browsers fold "\" to
	// "/", so "/\evil.com" (which url.Parse reads as an empty-host path)
	// becomes "//evil.com" — a protocol-relative redirect to another
	// origin. Checking the raw input closes that normalization gap.
	if strings.ContainsRune(v, '\\') {
		return ""
	}
	// The raw value must start with a single "/": a leading "//" (or the
	// backslash variant above) is protocol-relative and escapes the origin.
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	if u.Scheme != "" || u.Host != "" {
		return ""
	}
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return ""
	}
	return u.String()
}
