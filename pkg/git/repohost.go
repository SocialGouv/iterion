package git

import (
	"fmt"
	"net/url"
	"strings"
)

// RepoHost extracts the host from a git remote URL in either of the
// two forms git accepts: a scheme URL (https://host/path,
// ssh://user@host/path) or the scp-like shorthand ([user@]host:path).
// Case-insensitive callers should compare the result with EqualFold.
func RepoHost(repoURL string) (string, error) {
	s := strings.TrimSpace(repoURL)
	if i := strings.Index(s, "://"); i >= 0 {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("parse: %w", err)
		}
		host := u.Hostname()
		if host == "" {
			return "", fmt.Errorf("missing host in %q", repoURL)
		}
		return host, nil
	}
	// scp-like: `[user@]host:path`.
	colon := strings.Index(s, ":")
	if colon <= 0 {
		return "", fmt.Errorf("missing host in %q", repoURL)
	}
	host := s[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if host == "" {
		return "", fmt.Errorf("missing host in %q", repoURL)
	}
	return host, nil
}
