package server

import (
	"context"
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
	"github.com/SocialGouv/iterion/pkg/deeplink"
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
	// ssoErrProviderUnavailable is the banner of a provider resolution that
	// FAILED — an unreachable provider store, a secret the sealer could not
	// open (oidc.ErrProviderUnavailable). It is distinct from
	// ssoErrUnknownProvider on purpose: "no longer available" tells the user
	// to stop waiting for an SSO that is merely down, where "try again in a
	// moment" is the truth and the cause is in the server log.
	ssoErrProviderUnavailable = "provider_unavailable"
	// ssoErrPasswordChangeRequired refuses the SSO sign-in of an account an
	// administrator set pending a password change: the SPA names the exits
	// (the forced change, or the emailed reset for an SSO-only account).
	ssoErrPasswordChangeRequired = "password_change_required"
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

// redirectCallbackError answers a failed OIDC callback whose flow is known.
// A DESKTOP flow's refusal goes to the desktop's own loopback listener
// (?sso_error=), so the app stops waiting and says why at once, instead of
// leaving the reason in the browser tab while the desktop waits out its
// five-minute timeout (#2687); every other flow lands on the SPA login
// banner as before. The loopback target was validated at /start
// (safeDesktopRedirect: http on 127.0.0.1/[::1]/localhost only, an explicit
// port, no credentials), so this is no open redirect — it 302s only to the
// address the desktop itself named when it began the flow. pending zero
// (the flow could not be taken) is the SPA path.
func redirectCallbackError(w http.ResponseWriter, r *http.Request, pending oidc.PendingAuth, code string, extra url.Values) {
	if !pending.Desktop || pending.DesktopRedirect == "" {
		redirectSSOError(w, r, code, extra)
		return
	}
	q := url.Values{}
	q.Set("sso_error", code)
	for k, vs := range extra {
		for _, v := range vs {
			if v != "" {
				q.Add(k, v)
			}
		}
	}
	sep := "?"
	if strings.Contains(pending.DesktopRedirect, "?") {
		sep = "&"
	}
	http.Redirect(w, r, pending.DesktopRedirect+sep+q.Encode(), http.StatusFound)
}

// ssoErrorForAuth maps an auth-service login error to its SPA error code.
func ssoErrorForAuth(err error) string {
	switch {
	case errors.Is(err, auth.ErrLinkRequiresConsent):
		return ssoErrLinkRequired
	case errors.Is(err, auth.ErrSSORestricted):
		return ssoErrRestricted
	case errors.Is(err, auth.ErrPasswordChangeRequired):
		return ssoErrPasswordChangeRequired
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
			// Non-oracle by this function's contract, like the slug miss
			// above: a team list that cannot be read offers no tenant
			// rather than telling the caller the org exists.
			primary, _ := s.firstTeamInOrg(r.Context(), o.ID)
			push(primary)
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
	s.beginOIDCFlow(w, r, r.PathValue("provider"), auth.Identity{})
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
	// A link authenticates the account through an IdP identity from then on:
	// only a live session starts one (auth.Service.CheckDerivation), and the
	// callback asks again before it links.
	if _, err := s.checkDerivation(r.Context(), id, auth.MintSSOLink); err != nil {
		s.derivationError(w, r, err)
		return
	}
	s.beginOIDCFlow(w, r, r.PathValue("provider"), id)
}

// beginOIDCFlow resolves the connector, persists PendingAuth and redirects to
// the IdP authorize URL. link is the zero Identity for a sign-in flow and the
// caller's identity for the connect-from-settings flow: its account, and the
// session the callback checks again before it links.
func (s *Server) beginOIDCFlow(w http.ResponseWriter, r *http.Request, name string, link auth.Identity) {
	linkUserID := link.UserID
	c, tenantID, providerID, err := s.resolveConnector(r.Context(), name)
	if err != nil {
		// A resolution that FAILED is not the fact "unknown provider": the
		// start of a flow whose provider store is unreachable owes the
		// identity contract — 5xx, generic body, the cause in the log — so
		// the operator sees an outage instead of users reading a 404 as a
		// deleted SSO.
		if errors.Is(err, oidc.ErrProviderUnavailable) {
			s.identityFailure(w, r, fmt.Errorf("resolve the SSO provider %q: %w", name, err))
			return
		}
		httpError(w, http.StatusNotFound, "unknown provider")
		return
	}
	state, verifier, _, err := oidc.GenerateStateAndPKCE()
	if err != nil {
		s.identityFailure(w, r, fmt.Errorf("generate the OIDC state: %w", err))
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
		s.identityFailure(w, r, fmt.Errorf("build authorize URL: %w", err))
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
			s.identityFailure(w, r, fmt.Errorf("generate the agent binding: %w", err))
			return
		}
	}
	if err := s.oidcStates.Put(r.Context(), oidc.PendingAuth{
		Provider:              name,
		State:                 state,
		CodeVerifier:          verifier,
		RedirectURI:           redirectURI,
		NextURL:               next,
		IssuedAt:              time.Now().UTC(),
		AgentBinding:          binding,
		TenantID:              tenantID,
		OrgProviderID:         providerID,
		LinkUserID:            linkUserID,
		LinkSessionLineage:    link.SessionLineage,
		LinkSessionGeneration: link.SessionGeneration,
		Desktop:               desktop,
		DesktopRedirect:       desktopRedirect,
	}); err != nil {
		s.identityFailure(w, r, fmt.Errorf("persist state: %w", err))
		return
	}
	if binding != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     s.authCookieWriteName(oidcAgentBindingCookie),
			Value:    binding,
			Path:     s.agentBindingCookiePath("/api/auth/oidc/"),
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			// SameSite=Lax is required: the callback is a top-level GET
			// navigation from the IdP and Strict would block the cookie.
			// Lax alone is NOT sufficient, which is why the name carries the
			// __Host- prefix where it can. This code used to argue that a
			// cross-site script "can't set a cookie for iterion's origin
			// (same-origin policy)" — false on a host under a shared
			// registrable domain, where a sibling sets Domain=<parent> and
			// the browser sends it alongside ours. Tossing THIS cookie
			// defeats the RFC 9700 §4.7.1 login-CSRF guard and lands the
			// victim on the attacker's account.
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
	// Both spellings: a flow started before the migration holds the bare name,
	// and a single-use cookie that is not cleared is a replayable one.
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
	http.SetCookie(w, &http.Cookie{
		Name:  hostCookiePrefix + oidcAgentBindingCookie,
		Value: "",
		// A __Host- deletion is only honoured on the terms of its write.
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
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
		switch {
		case errors.Is(err, oidc.ErrProviderDisabled):
			code = ssoErrProviderDisabled
		case errors.Is(err, oidc.ErrProviderUnavailable):
			// A resolution that failed says nothing about the provider's
			// existence: the banner says "try again", and the cause — the
			// only place it survives — goes to the log.
			code = ssoErrProviderUnavailable
			if s.logger != nil {
				s.logger.Error("oidc callback: resolve the provider %s: %v", name, err)
			}
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
		// The flow is over, so its state is consumed here rather than left
		// replayable — and taking it is what tells a DESKTOP flow apart, so
		// the refusal reaches the desktop's loopback instead of sitting in
		// the browser tab while the app waits out its timeout (#2687).
		//
		// The flow the STATE names is the client waiting, whichever
		// provider's callback the error arrives on — the same thing the
		// accepted path below does with a state that disagrees
		// (pending.Provider != name is answered through
		// redirectCallbackError, with that pending). A refusal withheld
		// from the desktop that started the flow is the timeout this
		// branch exists to prevent, and the state is consumed either way,
		// so withholding it protects nothing. The disagreement is logged:
		// it names a provider that did not issue this callback.
		//
		// A read that did not complete is NOT a state consumed: it says so
		// in the log instead of leaving the claim "consumed" standing on a
		// state that may still be replayable.
		var flow oidc.PendingAuth
		if st := r.URL.Query().Get("state"); st != "" && s.oidcStates != nil {
			p, terr := s.oidcStates.Take(r.Context(), st)
			switch {
			case terr == nil:
				flow = p
				if p.Provider != name && s.logger != nil {
					s.logger.Warn("oidc callback error from %s: the state was issued by provider %q — the refusal is routed to the client that flow started", name, p.Provider)
				}
			case !errors.Is(terr, oidc.ErrStateNotFound):
				// A read that did not complete, like a provider
				// resolution that fails above: the cause survives
				// nowhere else, and this is a server fault, not a
				// client's.
				if s.logger != nil {
					s.logger.Error("oidc callback error from %s: read the flow's state: %v — the state was NOT consumed and may still be replayable", name, terr)
				}
			}
		}
		redirectCallbackError(w, r, flow, ssoErrProviderReturned, provQ)
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
		redirectCallbackError(w, r, pending, ssoErrStateExpired, provQ)
		return
	}
	// Verify the user-agent binding cookie matches the one issued at
	// /start (login-CSRF guard per RFC 9700 §4.7.1). Constant-time
	// compare avoids timing leaks on near-miss values. The cookie is
	// cleared regardless of outcome — single-use semantics.
	if pending.AgentBinding != "" {
		ckVal := s.sessionCookie(r, oidcAgentBindingCookie)
		clearOIDCAgentBindingCookie(w, s.cfg.CookieDomain, s.cfg.CookieSecure)
		if subtle.ConstantTimeCompare([]byte(ckVal), []byte(pending.AgentBinding)) != 1 {
			redirectSSOError(w, r, ssoErrAgentBinding, provQ)
			return
		}
	}
	ext, err := c.ExchangeCode(r.Context(), code, pending.RedirectURI, pending.CodeVerifier)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("oidc exchange failed for %s: %v", name, err)
		}
		redirectCallbackError(w, r, pending, ssoErrExchangeFailed, provQ)
		return
	}
	// Link flow: an authenticated user is attaching this SSO identity to their
	// existing account (started from /link/start). Attach and bounce back to
	// the account page instead of running login/signup.
	//
	// The target is the route SettingsPage is actually mounted on. It used to
	// be "/settings", which the studio has never routed: the bounce landed on
	// the authenticated catch-all, and the banner SettingsPage renders from
	// ?sso_linked / ?sso_link_error could not be shown — a link that failed
	// reported nothing at all.
	if pending.LinkUserID != "" {
		// The session that started the link must still be live: a sign-out,
		// "sign out everywhere", a password change or a disable landing during
		// the IdP round trip refuses the link. The access token that started it
		// may have expired meanwhile; its session is what is asked.
		started := auth.Identity{
			UserID:            pending.LinkUserID,
			Credential:        auth.CredentialAccessToken,
			SessionLineage:    pending.LinkSessionLineage,
			SessionGeneration: pending.LinkSessionGeneration,
		}
		if _, err := s.checkDerivation(r.Context(), started, auth.MintSSOLink); err != nil {
			if s.logger != nil {
				s.logger.Warn("oidc link refused for user %s via %s: the session that started it: %v", pending.LinkUserID, name, err)
			}
			http.Redirect(w, r, deeplink.Path("/account")+"?sso_link_error="+url.QueryEscape(linkErrorCode(err)), http.StatusFound)
			return
		}
		created, err := s.authSvc.LinkExternalToUser(r.Context(), ext, pending.LinkUserID, pending.LinkSessionLineage)
		if err != nil {
			// The write's answer may have been lost AFTER the row landed (the
			// lost-commit class): the row THIS flow wrote — same account, this
			// flow's session lineage — is removed before the caller is told
			// anything, exactly like the refusal path below. A row another
			// flow wrote is not ours to remove.
			if uerr := s.authSvc.RemoveFlowSSOLink(context.WithoutCancel(r.Context()), pending.LinkUserID, ext.Provider, ext.Subject, pending.LinkSessionLineage); uerr != nil {
				s.identityFailure(w, r, fmt.Errorf("remove the SSO link %s/%s of user %s after a write whose answer was lost: %w (write error: %v)", ext.Provider, ext.Subject, pending.LinkUserID, uerr, err))
				return
			}
			if s.logger != nil {
				s.logger.Warn("oidc link failed for user %s via %s: %v", pending.LinkUserID, name, err)
			}
			http.Redirect(w, r, deeplink.Path("/account")+"?sso_link_error="+url.QueryEscape(linkErrorCode(err)), http.StatusFound)
			return
		}
		// Asked again once the link exists: a "sign out everywhere" or a
		// disable whose record landed after the check above and before the
		// link was written may have removed the account's session links
		// without seeing it. A revocation recorded after this second check
		// sees the link. A refusal — or a check that could not complete —
		// removes the link this flow wrote before the caller is told
		// anything.
		if created {
			if _, err := s.checkDerivation(r.Context(), started, auth.MintSSOLink); err != nil {
				uerr := s.authSvc.RemoveFlowSSOLink(context.WithoutCancel(r.Context()), pending.LinkUserID, ext.Provider, ext.Subject, pending.LinkSessionLineage)
				if uerr != nil {
					s.identityFailure(w, r, fmt.Errorf("remove the SSO link %s/%s of user %s written from a session refused since: %w (refusal: %v)", ext.Provider, ext.Subject, pending.LinkUserID, uerr, err))
					return
				}
				if s.logger != nil {
					s.logger.Warn("oidc link of user %s via %s removed: the session that started it was refused once the link was written: %v", pending.LinkUserID, name, err)
				}
				http.Redirect(w, r, deeplink.Path("/account")+"?sso_link_error="+url.QueryEscape(linkErrorCode(err)), http.StatusFound)
				return
			}
		}
		http.Redirect(w, r, deeplink.Path("/account")+"?sso_linked="+url.QueryEscape(c.Display()), http.StatusFound)
		return
	}
	// Desktop flow: don't set browser cookies — and DON'T mint the session
	// here. The login is DEFERRED to the ticket's redemption (#2635): the
	// ticket carries what the redemption needs to mint it (the account, the
	// lineage the session will be born under, the landing team), and the
	// store holds the ticket only as its hash, so no credential ever rests
	// in desktop_sso_tickets and an unredeemed ticket leaves no live session
	// behind. The desktop redeems the ticket at /api/auth/desktop/exchange
	// over its native client (see desktop_sso.go), which is where the
	// refresh token first exists. The refresh token thus never enters a URL.
	if pending.Desktop {
		if s.desktopTickets == nil {
			redirectCallbackError(w, r, pending, ssoErrExchangeFailed, provQ)
			return
		}
		var d auth.DesktopLogin
		if pending.TenantID != "" {
			d, err = s.authSvc.LoginWithExternalForOrgDesktop(r.Context(), ext, pending.TenantID, pending.OrgProviderID, r.UserAgent(), s.clientIP(r))
		} else {
			d, err = s.authSvc.LoginWithExternalDesktop(r.Context(), ext, r.UserAgent(), s.clientIP(r))
		}
		if err != nil {
			// The refusal reaches the desktop that is waiting for it:
			// redirectCallbackError sends a desktop flow's sso_error to the
			// loopback listener, so an account pending a password change (or
			// any other refusal) fails the app's sign-in at once instead of
			// after its five-minute timeout (#2687).
			if s.logger != nil {
				s.logger.Warn("sso login failed via %s: %v", name, err)
			}
			redirectCallbackError(w, r, pending, ssoErrorForAuth(err), provQ)
			return
		}
		ticket, mintErr := s.desktopTickets.Mint(r.Context(), d)
		if mintErr != nil {
			if s.logger != nil {
				s.logger.Error("sso login via %s: mint the desktop ticket: %v", name, mintErr)
			}
			redirectCallbackError(w, r, pending, ssoErrExchangeFailed, provQ)
			return
		}
		sep := "?"
		if strings.Contains(pending.DesktopRedirect, "?") {
			sep = "&"
		}
		http.Redirect(w, r, pending.DesktopRedirect+sep+"ticket="+url.QueryEscape(ticket), http.StatusFound)
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
	s.setAuthCookies(w, res.AccessToken, res.AccessExpires, res.RefreshToken, res.RefreshExpires)
	target := pending.NextURL
	if target == "" {
		// The studio root, not "/": "/" is the product home now, and landing a
		// just-authenticated operator on the marketing page is not signing in.
		target = deeplink.Path("")
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
	// SessionLinked marks a link a signed-in session wrote from the account
	// settings: "sign out everywhere" and an admin disable remove those —
	// except that an account with no password keeps its oldest link, so it
	// never loses every sign-in method it had.
	SessionLinked bool `json:"session_linked"`
}

// handleListMySSOLinks returns the caller's connected SSO identities.
func (s *Server) handleListMySSOLinks(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	links, err := s.authSvc.ListSSOLinks(r.Context(), id.UserID)
	if err != nil {
		s.authError(w, r, err)
		return
	}
	out := struct {
		Links []ssoLinkView `json:"links"`
	}{Links: make([]ssoLinkView, 0, len(links))}
	for _, l := range links {
		v := ssoLinkView{Provider: l.Provider, ProviderUserID: l.ProviderUserID, Email: l.Email,
			SessionLinked: l.SessionLineage != ""}
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
		if errors.Is(err, auth.ErrLastSignInMethod) {
			s.httpErrorCode(w, r, http.StatusConflict, "last_sign_in_method", "%s", auth.ErrLastSignInMethod.Error())
			return
		}
		s.authError(w, r, err)
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
