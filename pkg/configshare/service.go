package configshare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// Service orchestrates a read/write through forge.FileClient over the pinned
// share record. It holds no forge credential — the caller mints a repo-scoped
// token and passes the matching FileClient, so token handling stays at the
// server layer where the forge connection lives.
type Service struct {
	Store Store
}

// NewService wires a Service over a share Store.
func NewService(store Store) *Service { return &Service{Store: store} }

// ProjectedRead reads the share's config file through fc and projects it to the
// share's visible paths — never the whole file. Returns the projection plus the
// current whole-file blob SHA (the if-match token for a later write).
func (svc *Service) ProjectedRead(ctx context.Context, fc forge.FileClient, sh *Share) (map[string]any, string, error) {
	slug, err := RepoSlug(sh.RepoURL)
	if err != nil {
		return nil, "", err
	}
	fr, err := fc.GetFile(ctx, slug, sh.ConfigPath, sh.RepoRef)
	if err != nil {
		return nil, "", err
	}
	full, err := parseConfig(fr.Content)
	if err != nil {
		return nil, "", err
	}
	return ProjectConfig(full, sh.VisiblePaths), fr.SHA, nil
}

// ApplyEdit validates + merges a patch and writes it back with an if-match SHA.
// expectSHA is the SHA the editor read; a mismatch — or a concurrent change
// detected by the forge — surfaces forge.ErrFileConflict so the caller returns
// the fresh projection for a diff rather than overwriting. message/author are
// server-derived (never editor input). Returns the new SHA + changed paths.
func (svc *Service) ApplyEdit(ctx context.Context, fc forge.FileClient, sh *Share, patch map[string]any, expectSHA, message, authorName, authorEmail string) (string, []string, error) {
	slug, err := RepoSlug(sh.RepoURL)
	if err != nil {
		return "", nil, err
	}
	fr, err := fc.GetFile(ctx, slug, sh.ConfigPath, sh.RepoRef)
	if err != nil {
		return "", nil, err
	}
	// A stale (or missing) expectSHA is a conflict — the caller must read, diff
	// and re-submit against the current SHA. Empty is treated as stale (the
	// handler also rejects an empty sha), so an omitted sha can't blind-write.
	if expectSHA != fr.SHA {
		return "", nil, forge.ErrFileConflict
	}
	full, err := parseConfig(fr.Content)
	if err != nil {
		return "", nil, err
	}
	merged, changed, err := ApplyPatch(full, patch, sh.AllowedPaths)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	for _, p := range changed {
		if v, ok := getPath(merged, strings.Split(p, ".")); ok {
			if err := ValidateLeaf(p, v); err != nil {
				return "", nil, fmt.Errorf("%w: %v", ErrValidation, err)
			}
		}
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("configshare: re-encode config: %w", err)
	}
	out = append(out, '\n')
	res, err := fc.PutFile(ctx, slug, forge.PutFile{
		Path: sh.ConfigPath, Content: out, Message: message, Branch: sh.RepoRef,
		PrevSHA: fr.SHA, AuthorName: authorName, AuthorEmail: authorEmail,
	})
	if err != nil {
		return "", nil, err
	}
	return res.SHA, changed, nil
}

// ValidatePaths checks that a share's allowed/visible entries are literal
// dotted JSON paths — no globs, no malformed or forbidden segment — and that
// every writable path is also readable. Enforced at mint so an operator can't
// grant an unbounded or dangerous scope.
func ValidatePaths(allowed, visible []string) error {
	check := func(list []string, kind string) error {
		for _, p := range list {
			if p == "" {
				return fmt.Errorf("%s path is empty", kind)
			}
			if strings.ContainsAny(p, "*") {
				return fmt.Errorf("%s path %q must not contain a glob", kind, p)
			}
			for _, seg := range strings.Split(p, ".") {
				if seg == "" || strings.Contains(seg, "/") {
					return fmt.Errorf("%s path %q is malformed", kind, p)
				}
				if forbiddenKeys[seg] || hardForbiddenSegments[seg] {
					return fmt.Errorf("%s path %q hits a forbidden field", kind, p)
				}
			}
		}
		return nil
	}
	if len(allowed) == 0 {
		return fmt.Errorf("at least one editable (allowed) path is required")
	}
	if err := check(allowed, "allowed"); err != nil {
		return err
	}
	if err := check(visible, "visible"); err != nil {
		return err
	}
	vis := make(map[string]bool, len(visible))
	for _, p := range visible {
		vis[p] = true
	}
	for _, p := range allowed {
		if !vis[p] {
			return fmt.Errorf("allowed path %q must also be a visible path", p)
		}
	}
	// Reject a grant that is a strict prefix of another (a subtree that swallows
	// a leaf). Belt to the runtime object-value rejection in collectPatchLeaves,
	// keeping grants leaf-shaped so per-field validation always runs.
	all := append(append([]string{}, allowed...), visible...)
	for _, a := range allowed {
		for _, b := range all {
			if a != b && strings.HasPrefix(b, a+".") {
				return fmt.Errorf("allowed path %q is a prefix of %q; grants must be leaves", a, b)
			}
		}
	}
	return nil
}

func parseConfig(b []byte) (map[string]any, error) {
	var full map[string]any
	if err := json.Unmarshal(b, &full); err != nil {
		return nil, fmt.Errorf("configshare: config file is not a JSON object: %w", err)
	}
	return full, nil
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// RepoSlug extracts the provider-native "owner/name" from a repo URL. Rejects a
// URL whose last two path segments aren't clean names, so a mis-stored RepoURL
// can't smuggle a path into the contents API call.
func RepoSlug(repoURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return "", fmt.Errorf("configshare: bad repo url %q: %w", repoURL, err)
	}
	p := strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/")
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return "", fmt.Errorf("configshare: repo url %q is not owner/name", repoURL)
	}
	owner, name := segs[len(segs)-2], segs[len(segs)-1]
	for _, s := range []string{owner, name} {
		if !repoNameRe.MatchString(s) || s == "." || s == ".." || strings.Contains(s, "..") {
			return "", fmt.Errorf("configshare: repo url %q has an illegal owner/name", repoURL)
		}
	}
	return owner + "/" + name, nil
}

var refRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ValidateRepoRef requires an explicit, well-formed branch/ref — never empty
// (so a write can't default to an unexpected branch) and no leading dash (flag
// injection) or "..".
func ValidateRepoRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("repo ref (branch) must be set explicitly")
	}
	if strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || !refRe.MatchString(ref) {
		return fmt.Errorf("illegal repo ref %q", ref)
	}
	return nil
}

// ValidateConfigPath requires a clean, repo-relative file path and refuses any
// traversal or a protected area (.git, .github/CI, Dockerfile, .env*) so a
// mis-minted share can never grant contents:write on CI or secrets.
func ValidateConfigPath(p string) error {
	if p == "" {
		return fmt.Errorf("config path must be set")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("config path must be a clean relative POSIX path")
	}
	if path.Clean(p) != p {
		return fmt.Errorf("config path must be normalized (%q)", path.Clean(p))
	}
	low := strings.ToLower(p)
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("config path must not traverse (..)")
		}
	}
	for _, bad := range []string{".git/", ".github/", ".gitlab-ci", "dockerfile", ".env"} {
		if strings.HasPrefix(low, bad) || strings.Contains(low, "/"+bad) {
			return fmt.Errorf("config path targets a protected area (%q)", bad)
		}
	}
	return nil
}
