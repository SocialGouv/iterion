package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// localForgeTokenSecret is the local secret a no-team launch reads its forge
// credential from. `forge_token` is the name the whole catalog already binds
// (bots/*/manifest.yaml) and the one `iterion secret set` documents, so an
// operator who can already run a PR bot locally has it configured.
const localForgeTokenSecret = "forge_token"

// prLaunchError separates the two things a refused PR launch can mean, because
// the board lane must dispose of them differently: a REFUSAL is a decision
// about this pull request or this deployment's configuration (a fork, an
// unparsable URL, no connection, a rejected credential) and re-asking changes
// nothing; an UNAVAILABLE is a failure to ask at all (5xx, a timeout, a
// network blip) and the same card would have launched a minute later.
//
// Collapsing them is what filed a card `blocked` — an operator-facing terminal
// flag reconcileDeadPointer refuses to reclassify — for a 30-second forge
// hiccup, on a lane that made no forge call at all before this guard existed.
type prLaunchError struct {
	msg   string
	cause error
	// retryable is the CONSERVATIVE half: only errors we could not turn into
	// a decision land here, never a refusal.
	retryable bool
}

func (e *prLaunchError) Error() string { return e.msg }
func (e *prLaunchError) Unwrap() error { return e.cause }

// prLaunchUnavailable reports whether err is a failure to ASK rather than a
// decision — the only class a caller may retry.
func prLaunchUnavailable(err error) bool {
	var pe *prLaunchError
	return errors.As(err, &pe) && pe.retryable
}

// classifyPRLookupError decides which of the two a forge lookup failure is.
//
// The allowlist runs in the PERMANENT direction, never "untyped ⇒ transient":
// forge.StatusErr types only 401/403/404, so 400/422/429/5xx all arrive as one
// untyped "HTTP <code>" string. Treating the typed sentinels as permanent and
// everything else as retryable therefore costs an untyped 400/422 a few
// attempts before it is filed — visible in the same place, just later — while
// a 5xx or a timeout self-heals, which is the failure this exists for.
//
// Known residual: GitHub answers 403 for BOTH a permission denial and a
// secondary rate limit, so a rate-limited card is filed terminally. Splitting
// them needs response headers the admin clients do not surface.
func classifyPRLookupError(op string, err error) error {
	permanent := errors.Is(err, forge.ErrUnauthorized) ||
		errors.Is(err, forge.ErrForbidden) ||
		errors.Is(err, forge.ErrHookNotFound) ||
		errors.Is(err, forge.ErrPermissionsNotGranted)
	return &prLaunchError{
		msg:       fmt.Sprintf("PR launch: %s: %v", op, err),
		cause:     err,
		retryable: !permanent,
	}
}

// prLaunchRefusal builds the terminal half: a decision, never retried.
func prLaunchRefusal(format string, args ...any) error {
	return &prLaunchError{msg: fmt.Sprintf(format, args...)}
}

// verifyPRHeadInBaseRepo is issue #702's guard, and the ONE place the launch
// surfaces apply it: the head branch a run will check out must be PROVEN to
// live in the base repo. When it does not, the checkout either misses or
// silently resolves to a SAME-NAMED branch on the base repo — and a
// code-pushing bot then commits onto that. The decision is
// forge.PullRef.SameRepoAs + forkGuardRefusal, the same choke point the
// webhook lanes route through (#683); only the credential the lookup runs
// under differs by surface, which is why the client arrives as a parameter.
func verifyPRHeadInBaseRepo(ctx context.Context, gc forgeGateClient, repo string, number int) error {
	pr, err := gc.GetPullRequest(ctx, repo, number)
	if err != nil {
		return classifyPRLookupError("resolve head repository", err)
	}
	if refusal := forkGuardRefusal(pr.SameRepoAs(repo), false, pr.HeadRepoFullName); refusal != "" {
		return prLaunchRefusal("PR launch: %s", refusal)
	}
	return nil
}

// verifyLocalPRHead applies that same guard on a launch carrying NO team
// identity — a local studio, whose DisableAuth identity has an empty TeamID
// (requireAuth) and which wires no forge connections. The hazard does not
// depend on a team: the launch pair is still <base>.CloneURL + the PR's head
// branch, and a same-named base-repo branch is hit whether or not the server
// minted a publish grant. So the surface resolves the head through the
// operator's OWN forge credential instead of a Connection.
//
// It refuses when it cannot ask — deliberately, and never silently: same-repo
// is proven, never assumed (#702). The refusals below say what is missing and
// how to supply it, because "explain the limitation" is the contract here; a
// bypass would leave the guard advertised and inert.
//
// Only policy and the publish grant stay team-scoped: neither exists without a
// Connection, and injectForgePublishVars no-ops on a nil connection store.
func (s *Server) verifyLocalPRHead(ctx context.Context, prURL string) error {
	host, repo, number, err := forge.ParsePullURL(prURL)
	if err != nil {
		return prLaunchRefusal("PR launch: %v", err)
	}
	provider, base, ok := providerForPullURL(prURL, host)
	if !ok {
		return prLaunchRefusal("PR launch: cannot tell which forge serves %s, so the PR's head repository cannot be verified — "+
			"iterion refuses rather than assume the head branch lives in %s (a fork's branch can silently resolve to a same-named branch there). "+
			"Launch from a checkout of the PR branch with `iterion run` instead", prURL, repo)
	}
	token, err := s.localForgeToken(ctx, base, host)
	if err != nil {
		return prLaunchRefusal("PR launch: reading the local %s secret to verify %s: %v", localForgeTokenSecret, prURL, err)
	}
	if token == "" {
		return prLaunchRefusal("PR launch: no credential can verify that %s's head branch lives in %s — this server holds no forge connection for %s "+
			"and no local `%s` secret usable there. iterion refuses rather than assume same-repo: a fork's head branch can silently resolve to a "+
			"same-named branch in the base repo and a code-pushing bot would commit onto it. Add one with `iterion secret set %s --hosts %s` "+
			"(the host pin is what authorises a credential for a forge iterion cannot recognise on its own, so one is never offered to a host a "+
			"launch merely named), or launch from a checkout of the PR branch with `iterion run`",
			prURL, repo, host, localForgeTokenSecret, localForgeTokenSecret, host)
	}
	gc, err := s.localGateClientFor(ctx, provider, base, token)
	if err != nil {
		return classifyPRLookupError("resolve forge client", err)
	}
	if gc == nil {
		return prLaunchRefusal("PR launch: provider %s cannot resolve pull requests", provider)
	}
	return verifyPRHeadInBaseRepo(ctx, gc, repo, number)
}

// localGateClientFor builds the PR-reading client for the no-team lane from a
// raw token, since there is no Connection to mint one from. It goes through
// the SAME forgeGateClientFor test seam as the team lane so a test cannot
// prove one guard while the other rots.
func (s *Server) localGateClientFor(ctx context.Context, provider forge.Provider, baseURL, token string) (forgeGateClient, error) {
	if s.forgeGateClientFor != nil {
		return s.forgeGateClientFor(ctx, forge.Connection{Provider: provider, ForgeBaseURL: baseURL})
	}
	admin, err := s.forgeAdminForToken(provider, baseURL, token)
	if err != nil {
		return nil, err
	}
	gc, ok := admin.(forgeGateClient)
	if !ok {
		return nil, nil
	}
	return gc, nil
}

// localForgeToken opens the operator's local `forge_token` secret for one
// forge ORIGIN. An empty return is "none usable here", not an error — the
// caller turns that into the explanatory refusal above.
//
// Reads the store through localSecretStore(), not the field: it is hot-swapped
// on a project switch under stateMu.
func (s *Server) localForgeToken(ctx context.Context, origin, host string) (string, error) {
	store := s.localSecretStore()
	if store == nil || s.sealer == nil {
		return "", nil
	}
	creds, err := secrets.ResolveLocalCredentials(ctx, store, s.sealer, []string{localForgeTokenSecret}, s.logger)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(creds.Generic[localForgeTokenSecret])
	if token == "" {
		return "", nil
	}
	allowed := creds.GenericHosts[localForgeTokenSecret]
	if !localTokenPermittedAt(allowed, origin, host) {
		if s.logger != nil && len(allowed) > 0 {
			s.logger.Warn("PR launch: the local %s secret is pinned to %v and cannot be used against %s — not sending it there",
				localForgeTokenSecret, allowed, host)
		} else if s.logger != nil {
			s.logger.Warn("PR launch: the local %s secret carries no host pin, and %s is not an origin iterion recognises on its own — "+
				"not sending it there; authorise it with `iterion secret set %s --hosts %s`",
				localForgeTokenSecret, origin, localForgeTokenSecret, host)
		}
		return "", nil
	}
	return token, nil
}

// localTokenPermittedAt is the ONE answer to "may the operator's local
// forge_token be sent to this origin". The destination is named by the
// REQUEST's own pr_url, so two independent grounds admit it and nothing else
// does:
//
//   - PINNED — the secret's own AllowedHosts bounds it, the same egress rule
//     secretguard applies when materialising a placeholder. A token pinned to
//     one forge is not sent to another just because a launch named a PR there.
//   - UNPINNED — the secret supplied no bound, so iterion's own trust is all
//     that is left. `iterion secret set` leaves --hosts unset by default, so
//     this is the COMMON shape; before this rule it meant a stored forge
//     write credential went to whatever origin a launch's pr_url named, on the
//     one surface (DisableAuth, no team) that holds such a credential.
//     providerForPullURL is no help there: it resolves a provider from the PR
//     PATH SHAPE for any host at all.
func localTokenPermittedAt(allowed []string, origin, host string) bool {
	if len(allowed) == 0 {
		return trustedUnpinnedForgeOrigin(origin)
	}
	return hostMatchesPin(allowed, host)
}

// trustedUnpinnedForgeOrigins are the only destinations an UNPINNED
// forge_token is offered to: origins iterion recognises independently of the
// caller's URL. EXACT canonical origins — never a suffix test, which admits
// github.com.evil.io, and never the PR path shape.
//
// The scheme is part of the origin because the GitLab and Forgejo
// constructors keep whatever base they are handed (`strings.TrimRight`, no
// canonicalisation), so http://gitlab.com/... would put the token on the wire
// in plaintext. GitHub's own constructor maps both schemes to
// https://api.github.com, so the rule is stricter than that one path needs —
// one rule is worth more than three exceptions.
//
// A `www.` alias or a port fails closed to "needs a pin", which is a refusal
// naming the exact command that fixes it.
var trustedUnpinnedForgeOrigins = map[string]struct{}{
	"https://github.com":   {},
	"https://gitlab.com":   {},
	"https://codeberg.org": {},
}

func trustedUnpinnedForgeOrigin(origin string) bool {
	_, ok := trustedUnpinnedForgeOrigins[strings.ToLower(strings.TrimRight(strings.TrimSpace(origin), "/"))]
	return ok
}

// hostMatchesPin reports whether host matches at least one of a secret's
// AllowedHosts patterns — exactly, or as a parent domain ("github.com" permits
// "api.github.com"). Same rule as secretguard.hostAllowed, which is
// unexported, MINUS its empty-list case: on this lane "no pin" no longer means
// "any host", and localTokenPermittedAt owns that decision.
//
// Known divergence from secretguard.hostAllowed, fail-closed and deliberately
// left alone here: it does not strip a port or IPv6 brackets, while
// forge.ParsePullURL returns u.Host WITH the port. So a token pinned to
// `forge.example.com` does not cover a pr_url on `forge.example.com:8443` —
// the operator pins the host:port form. Exporting one shared helper is the
// right closure and is bigger than this guard.
func hostMatchesPin(allowed []string, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && (a == host || strings.HasSuffix(host, "."+a)) {
			return true
		}
	}
	return false
}

// providerForPullURL infers which forge API speaks for a pull-request web URL
// on a surface that holds no Connection to read the provider off. Two signals,
// host first — the SaaS hosts are unambiguous — then the PR path shape, which
// names every self-hosted instance: GitLab's "/-/merge_requests/<n>",
// Forgejo/Gitea's "/pulls/<n>", GitHub Enterprise's "/pull/<n>". Those are the
// same three shapes forge.ParsePullURL accepts, so a URL that parsed there
// always lands on one of them. The path shape only ever serves a PINNED
// credential now: an unpinned one reaches the canonical SaaS origins alone
// (localTokenPermittedAt), which the host switch below already covers.
//
// Also returns the forge's WEB base URL, which is what every provider
// constructor takes. The host is CANONICALISED to lower case there: url.Parse
// lowercases the scheme but not the host, and a mixed-case one falls off
// github.APIBaseFor's exact switch — "https://GitHub.com" would be read as a
// GitHub Enterprise host and the lookup sent to /api/v3, refusing a perfectly
// good PR URL with the wrong cause. Hosts are case-insensitive (RFC 3986), so
// this is a canonicalisation, not a policy.
func providerForPullURL(raw, host string) (forge.Provider, string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	base := u.Scheme + "://" + strings.ToLower(u.Host)
	switch strings.ToLower(host) {
	case "github.com", "www.github.com":
		return forge.ProviderGitHub, base, true
	case "gitlab.com", "www.gitlab.com":
		return forge.ProviderGitLab, base, true
	case "codeberg.org":
		return forge.ProviderForgejo, base, true
	}
	switch {
	case strings.Contains(u.Path, "/merge_requests/"):
		return forge.ProviderGitLab, base, true
	case strings.Contains(u.Path, "/pulls/"):
		return forge.ProviderForgejo, base, true
	case strings.Contains(u.Path, "/pull/"):
		return forge.ProviderGitHub, base, true
	}
	return "", "", false
}
