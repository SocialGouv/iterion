package git

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// AuthEnv returns env with a forge token added as an extraHeader git config
// entry (`AUTHORIZATION: Basic base64("oauth2:"+token)`) bound to the origin
// of remoteURL: the key is `http.<scheme>://<host>/.extraHeader`, carried
// through GIT_CONFIG_COUNT / GIT_CONFIG_KEY_<n> / GIT_CONFIG_VALUE_<n>
// (git >= 2.31). An empty token returns env unchanged.
//
// The credential travels in the environment, never in argv: a process's
// argv is world-readable through /proc/<pid>/cmdline, its environment only
// to its own user. Nothing is written to the repository's config either.
// `oauth2` as the basic-auth username is accepted by GitLab, GitHub and
// Forgejo alike.
//
// The header goes to the forge origin only. git and git-lfs both match the
// origin-scoped key against each request's URL, so an LFS storage host the
// batch API names, or a host an `lfs.url` in the repository's .lfsconfig
// names, never receives it. A remote that is not http(s) — ssh, scp-like,
// a local path, file:// — gets no header at all: git-lfs reads http.* config
// for the https requests it makes on behalf of an ssh remote too. An
// http(s) remoteURL that does not parse, or names no host, is an error. An
// operator url.<base>.insteadOf that rewrites the forge URL onto another
// host leaves the key unmatched: the request goes out without the header
// and the forge refuses it.
//
// The entry is appended after any GIT_CONFIG_* entries env already carries
// (an operator's insteadOf, a sandbox's safe.directory): the count in force
// is read — the last GIT_CONFIG_COUNT wins, as it does for the child — and
// the header takes the next index. The count is parsed as git parses it:
// empty is 0, leading whitespace is skipped, anything else that is not a
// non-negative integer (a blank value, trailing characters) is an error,
// because renumbering over it would clobber the entries it numbers, and git
// itself refuses such a count.
func AuthEnv(env []string, remoteURL, token string) ([]string, error) {
	if token == "" {
		return env, nil
	}
	lower := strings.ToLower(remoteURL)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return env, nil
	}
	// The URL stays out of both errors: a malformed one may carry userinfo.
	u, err := url.Parse(remoteURL)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("git: the http(s) remote URL does not parse, so the forge credential cannot be scoped to its origin: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("git: the http(s) remote URL names no host to scope the forge credential to")
	}
	next, err := nextConfigIndex(env)
	if err != nil {
		return nil, err
	}
	header := "AUTHORIZATION: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+token))
	out := make([]string, 0, len(env)+3)
	out = append(out, env...)
	return append(out,
		fmt.Sprintf("GIT_CONFIG_KEY_%d=http.%s://%s/.extraHeader", next, strings.ToLower(u.Scheme), u.Host),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", next, header),
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", next+1),
	), nil
}

// nextConfigIndex reads the GIT_CONFIG_COUNT in force in env (the last one
// wins) with git's own parsing: "" is 0; otherwise strtoul semantics —
// leading whitespace and a sign skipped, no trailing characters.
func nextConfigIndex(env []string) (int, error) {
	raw := ""
	for _, kv := range env {
		if key, value, _ := strings.Cut(kv, "="); key == "GIT_CONFIG_COUNT" {
			raw = value
		}
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimLeft(raw, " \t\n\v\f\r"))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("git: GIT_CONFIG_COUNT=%q is not a non-negative integer; the forge credential cannot be appended to the git config it numbers", raw)
	}
	return n, nil
}
