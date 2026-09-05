package server

import (
	"context"
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
		return fmt.Errorf("PR launch: resolve head repository: %w", err)
	}
	if refusal := forkGuardRefusal(pr.SameRepoAs(repo), false, pr.HeadRepoFullName); refusal != "" {
		return fmt.Errorf("PR launch: %s", refusal)
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
		return fmt.Errorf("PR launch: %w", err)
	}
	provider, base, ok := providerForPullURL(prURL, host)
	if !ok {
		return fmt.Errorf("PR launch: cannot tell which forge serves %s, so the PR's head repository cannot be verified — "+
			"iterion refuses rather than assume the head branch lives in %s (a fork's branch can silently resolve to a same-named branch there). "+
			"Launch from a checkout of the PR branch with `iterion run` instead", prURL, repo)
	}
	token, err := s.localForgeToken(ctx, host)
	if err != nil {
		return fmt.Errorf("PR launch: reading the local %s secret to verify %s: %w", localForgeTokenSecret, prURL, err)
	}
	if token == "" {
		return fmt.Errorf("PR launch: no credential can verify that %s's head branch lives in %s — this server holds no forge connection for %s "+
			"and no local `%s` secret usable there. iterion refuses rather than assume same-repo: a fork's head branch can silently resolve to a "+
			"same-named branch in the base repo and a code-pushing bot would commit onto it. Add one with `iterion secret set %s`, or launch from a "+
			"checkout of the PR branch with `iterion run`", prURL, repo, host, localForgeTokenSecret, localForgeTokenSecret)
	}
	gc, err := s.localGateClientFor(ctx, provider, base, token)
	if err != nil {
		return fmt.Errorf("PR launch: resolve forge client: %w", err)
	}
	if gc == nil {
		return fmt.Errorf("PR launch: provider %s cannot resolve pull requests", provider)
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
// forge host. An empty return is "none usable here", not an error — the caller
// turns that into the explanatory refusal above.
//
// The secret's own AllowedHosts lock is enforced: a token pinned to one forge
// must not be sent to another just because a launch named a PR there. That is
// the same egress rule secretguard applies when materialising a placeholder.
func (s *Server) localForgeToken(ctx context.Context, host string) (string, error) {
	if s.localSecrets == nil || s.sealer == nil {
		return "", nil
	}
	creds, err := secrets.ResolveLocalCredentials(ctx, s.localSecrets, s.sealer, []string{localForgeTokenSecret}, s.logger)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(creds.Generic[localForgeTokenSecret])
	if token == "" {
		return "", nil
	}
	if allowed := creds.GenericHosts[localForgeTokenSecret]; !hostPermittedBy(allowed, host) {
		if s.logger != nil {
			s.logger.Warn("PR launch: the local %s secret is pinned to %v and cannot be used against %s — not sending it there",
				localForgeTokenSecret, allowed, host)
		}
		return "", nil
	}
	return token, nil
}

// hostPermittedBy reports whether host is covered by a secret's AllowedHosts.
// Empty = unpinned (any host); a pattern matches exactly or as a parent domain
// ("github.com" permits "api.github.com"). Same rule as
// secretguard.hostAllowed, which is unexported.
func hostPermittedBy(allowed []string, host string) bool {
	if len(allowed) == 0 {
		return true
	}
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
// always lands on one of them. Also returns the forge's WEB base URL, which is
// what every provider constructor takes.
func providerForPullURL(raw, host string) (forge.Provider, string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	base := u.Scheme + "://" + u.Host
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
