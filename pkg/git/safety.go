package git

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ValidateRelPath accepts a path coming from an HTTP query and verifies
// it stays inside the run's working directory. It rejects absolute paths,
// `..` traversal, and NUL bytes.
//
// The accepted form is a forward-slash relative path. Callers should pass
// the value straight through to git/os.ReadFile after validation; we do
// not normalise to OS separators here because git itself uses forward
// slashes on every platform.
func ValidateRelPath(p string) error {
	if p == "" {
		return fmt.Errorf("git: path must not be empty")
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("git: path contains null byte")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("git: path must be relative")
	}
	// The wire format is slash-separated on every platform. Reject
	// backslashes before applying host-OS path rules so a Windows drive path
	// such as C:\secret is refused consistently even when the server runs on
	// Linux (where backslash would otherwise be treated as a normal byte).
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("git: path must use forward slashes")
	}
	// Reject a leading dash. showAt (range.go) passes `ref:<relPath>`
	// as a single positional arg to `git show`; a path starting with
	// "-" would be parsed as a git flag (e.g. `git show HEAD:-v` ⇒
	// verbose mode), leaking unrelated output to the caller.
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("git: path %q must not start with '-' (would be parsed as a flag)", p)
	}
	// filepath.IsLocal (Go 1.20+) rejects "..", "" segments, drive
	// letters, and other escape attempts using the OS rules. We
	// normalise to OS separators just for this check so the same input
	// is judged identically on Windows and Linux.
	osPath := filepath.FromSlash(p)
	if !filepath.IsLocal(osPath) {
		return fmt.Errorf("git: path %q escapes working directory", p)
	}
	// Reject any ".git" path component. filepath.IsLocal allows ".git/config",
	// ".git/HEAD", etc. (they don't escape the root), but the read/diff
	// endpoints serve working-tree files, never the repository's internal git
	// dir — exposing it would leak config, refs, hooks and packed objects.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".git" {
			return fmt.Errorf("git: path %q must not reference the .git directory", p)
		}
	}
	return nil
}

// branchNameAllowed accepts an alphanumeric, hyphen, underscore, dot
// or slash sequence. The leading byte must be alphanumeric so a value
// starting with `-` can never reach `git branch` as a positional that
// might be re-parsed as a flag (defense in depth — callers should also
// pass `--` to git, but this catches the bug earlier with a friendly
// error).
var branchNameAllowed = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ValidateCloneSource gates the URL passed to `git clone`. The `--` sentinel
// in ShallowClone already blocks command-line flag injection, but it does NOT
// constrain git's URL transports: git supports remote-helper transports such
// as `ext::` (which executes an arbitrary command) and `file://` (which clones
// an arbitrary local repository). Those are a security boundary issue the
// moment a less-trusted surface — marketplace catalogs, webhooks — can feed an
// install source, so we allow only a small set of known-safe transports rather
// than reject a blocklist that git keeps extending.
//
// Accepted:
//   - `https://…` Git URLs.
//   - `ssh://…` Git URLs.
//   - scp-like SSH syntax `[user@]host:path` (e.g. `git@github.com:org/repo.git`).
//
// Rejected (with a clear error): the remote-helper marker `::` in any position
// (`ext::…`, `<transport>::address`), and every other URL scheme — `file://`,
// `git://` and `http://` (cleartext/unauthenticated), `ftp://`, etc. A bare
// local or relative path is also rejected here: intentional local-directory
// installs are handled upstream by botinstall.resolveRepoRoot (os.Stat+IsDir)
// before a source ever reaches the clone path, so anything left that looks
// like a path is not a recognised git transport.
//
// Edge cases left deliberately permissive (not security regressions — none is
// `ext::`/`file://`): a Windows drive path like `C:\repo` matches the scp-like
// shape, and an `ssh://` URL without a user is accepted. iterion targets Linux
// and local directories are diverted upstream, so widening the rules to chase
// these would add complexity without closing a real hole.
func ValidateCloneSource(src string) error {
	s := strings.TrimSpace(src)
	if s == "" {
		return fmt.Errorf("git: clone url is empty")
	}
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("git: clone url contains null byte")
	}
	// `::` is git's remote-helper transport marker (`ext::`, `transport::addr`).
	// Reject it in any position before scheme parsing so `ext::sh -c …` cannot
	// slip through as a path-shaped value.
	if strings.Contains(s, "::") {
		return fmt.Errorf("git: clone source %q uses an unsupported transport (remote-helper transports such as ext:: are not allowed; use an https:// or ssh git URL)", src)
	}
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := strings.ToLower(s[:i])
		switch scheme {
		case "https", "ssh":
			return nil
		default:
			return fmt.Errorf("git: clone source %q uses an unsupported transport %q (only https:// and ssh git URLs are allowed)", src, scheme)
		}
	}
	// No explicit scheme: the only accepted form is scp-like SSH, which git
	// recognises when a colon appears before the first slash (`host:path`).
	colon := strings.Index(s, ":")
	slash := strings.Index(s, "/")
	if colon > 0 && (slash == -1 || colon < slash) {
		host := s[:colon]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if host != "" {
			return nil
		}
	}
	return fmt.Errorf("git: clone source %q is not a supported git transport (only https:// and ssh git URLs are allowed; install a local bundle by its directory path instead)", src)
}

// ValidateBranchName accepts a branch name coming from a user-controlled
// surface (`--branch-name` CLI flag, Launch API, studio modal) and
// rejects forms that could either confuse `git branch` flag parsing or
// be rejected by `git check-ref-format` downstream — failing early with
// a clear error rather than surfacing a noisy git stderr to the caller.
//
// The rules are intentionally tighter than git's own check-ref-format:
// we want a small allowlist (letters, digits, `.`, `_`, `/`, `-`) so
// every accepted value also passes git's checks. Combined with the
// `--` sentinel used by callers, this prevents flag injection through
// the storage-branch name.
func ValidateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("git: branch name must not be empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("git: branch name must be at most 255 bytes")
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("git: branch name contains null byte")
	}
	if !branchNameAllowed.MatchString(name) {
		return fmt.Errorf("git: branch name %q must match [A-Za-z0-9][A-Za-z0-9._/-]* (no leading -/./_, no spaces or special chars)", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("git: branch name %q must not contain '..'", name)
	}
	if strings.Contains(name, "//") {
		return fmt.Errorf("git: branch name %q must not contain '//'", name)
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("git: branch name %q must not end with '/' or '.'", name)
	}
	if strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("git: branch name %q must not end with '.lock'", name)
	}
	return nil
}
