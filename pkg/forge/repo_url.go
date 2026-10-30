package forge

import "strings"

// CloneURLFor derives the HTTPS clone URL of a repo hosted on a forge
// instance. All three supported providers (GitHub, GitLab, Forgejo)
// share the <base>/<owner>/<repo>.git shape — GitLab subgroups are just
// deeper full-name paths.
func CloneURLFor(baseURL, repoFullName string) string {
	return WebURLFor(baseURL, repoFullName) + ".git"
}

// WebURLFor derives the repo's browser URL on its forge instance.
func WebURLFor(baseURL, repoFullName string) string {
	return strings.TrimSuffix(baseURL, "/") + "/" + strings.Trim(repoFullName, "/")
}

// CanonicalRepoSlug canonicalises a repo path to the slug the forge meters
// by: the trailing ".git" — any case, a copy-pasted URL recases the suffix
// as readily as the slug — and the stray slashes a clone URL can carry
// ("acme/widgets.git/", "acme/widgets.git//", "/acme/widgets.git",
// "acme/widgets.git.git") all fold back to "acme/widgets", the same key
// whichever spelling arrived. The fold runs to a FIXED point: a single pass
// turns "acme/widgets.git/" into "acme/widgets.git" and stops there — a key
// no quota aims at, which is exactly the split this function exists to
// prevent. The slug's own case survives the fold: "Acme/Widgets" is how the
// forge writes the repository (a webhook payload carries it byte-for-byte),
// so the derivation must not re-case what it stamps; the case is folded
// where the identity is READ instead (budgetfloor's quota match, credusage's
// repository read, forge.SameRepo) — a forge resolves case-variant slugs to
// one repository, so a spelling is never a second repo.
//
// Every derivation that turns a clone-URL path into a quota/meter key goes
// through it — a derivation keeping its own trim is how one repository comes
// to be charged under two keys, each reading its quota as half-used.
func CanonicalRepoSlug(path string) string {
	p := strings.Trim(path, "/")
	for {
		prev := p
		if n := len(p); n >= 4 && strings.EqualFold(p[n-4:], ".git") {
			p = p[:n-4]
		}
		if trimmed := strings.Trim(p, "/"); trimmed != p {
			p = trimmed
		}
		if p == prev {
			return p
		}
	}
}

// ProjectPathFromCloneURL canonicalises a repo clone URL back to the forge
// slug ("owner/repo", GitLab subgroups included) — the inverse of
// CloneURLFor, and the identity pkg/credusage meters a run's spend against
// (store.Run.ProjectPath). The case is preserved here: the slug is stamped
// byte-for-byte as the forge writes it, and the case folds where the
// identity is read (CanonicalRepoSlug's doc names the reads).
//
// The host is dropped, not kept: the slug is the meter's whole key and is
// not unique across forge hosts (#950's finding 5), so two deployments'
// "acme/widgets" share one quota key by design — the same reading every
// launch surface's stamped slug already produces. What this derivation
// CANNOT see is a forge hosted under a sub-path base
// ("https://host/gitlab/owner/repo.git"): without the connection's BaseURL
// the base's own path is indistinguishable from a subgroup, so a caller that
// holds the forge's own slug (a webhook payload's project path) must prefer
// it and treat this as the fallback for a bare clone URL. The scheme forms
// handled are the ones a card or a launch request can carry: https://, http://,
// ssh://, the scp-like git@host:owner/repo.git, and a bare slug (returned
// canonicalised, making the function idempotent on its own output).
func ProjectPathFromCloneURL(cloneURL string) string {
	u := strings.TrimSpace(cloneURL)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		// scheme://authority/path — the slug is the path minus the
		// authority (host[:port], userinfo included).
		u = u[i+3:]
		if i := strings.Index(u, "/"); i >= 0 {
			return CanonicalRepoSlug(u[i+1:])
		}
		return ""
	}
	if strings.HasPrefix(u, "git@") {
		// scp-like syntax: the path starts after the host's colon.
		if i := strings.Index(u, ":"); i >= 0 {
			return CanonicalRepoSlug(u[i+1:])
		}
		return ""
	}
	// A bare slug (or a host-less path): nothing to strip. Idempotent on
	// the function's own output.
	return CanonicalRepoSlug(u)
}
