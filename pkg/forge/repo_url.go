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

// ProjectPathFromCloneURL canonicalises a repo clone URL back to the forge
// slug ("owner/repo", GitLab subgroups included) — the inverse of
// CloneURLFor, and the identity pkg/credusage meters a run's spend against
// (store.Run.ProjectPath). The case is preserved: the meter keys on the slug
// byte-for-byte as the forge writes it, and GitHub's "Owner/Repo" is not
// "owner/repo".
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
	trim := func(s string) string {
		return strings.Trim(strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git"), "/")
	}
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		// scheme://authority/path — the slug is the path minus the
		// authority (host[:port], userinfo included).
		u = u[i+3:]
		if i := strings.Index(u, "/"); i >= 0 {
			return trim(u[i+1:])
		}
		return ""
	}
	if strings.HasPrefix(u, "git@") {
		// scp-like syntax: the path starts after the host's colon.
		if i := strings.Index(u, ":"); i >= 0 {
			return trim(u[i+1:])
		}
		return ""
	}
	// A bare slug (or a host-less path): nothing to strip. Idempotent on
	// the function's own output.
	return trim(u)
}
