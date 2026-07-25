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
