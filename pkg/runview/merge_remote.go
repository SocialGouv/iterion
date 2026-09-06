package runview

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// This file gives repo-targeted runs a merge path. Their runner workspace is
// wiped when the run returns, so unlike local runs there is no checkout for
// the merge pipeline to operate in — the storage branch only exists on the
// forge. The service materialises a dedicated server-side clone, runs the
// exact same merge pipeline a local run gets, then pushes the advanced
// target branch back to the forge.

// ForgeTokenResolver supplies the forge credential for a repo-targeted
// run's server-side merge clone and push. Installed by the server (which
// can reach the forge secret store) via WithForgeTokenResolver; nil on
// local studios, where merges happen in the user's own checkout.
type ForgeTokenResolver func(ctx context.Context, r *store.Run) (string, error)

// mergeGitTimeout bounds each git invocation of the merge clone. Clones and
// pushes travel over the forge network; a hung remote must not pin the HTTP
// handler forever.
const mergeGitTimeout = 120 * time.Second

// repoTargetedMergeRoot is where the server-side merge clone for runID
// lives. Stable across calls on purpose: conflict resolution spans several
// HTTP round-trips and each one must see the same tree. With a
// filesystem store the clone sits under the store; the cloud service has
// no local store dir, so it falls back to the OS temp dir — the clone is
// re-creatable from the forge at any time, so losing it to a pod restart
// only costs a re-clone.
func (s *Service) repoTargetedMergeRoot(runID string) string {
	if runID == "" {
		return ""
	}
	if s.storeDir == "" {
		return filepath.Join(os.TempDir(), "iterion-merges", runID)
	}
	return filepath.Join(s.storeDir, "merges", runID)
}

// hasRepoTargetedMergeRoot reports whether a materialised merge clone
// already exists for runID.
func (s *Service) hasRepoTargetedMergeRoot(runID string) bool {
	dir := s.repoTargetedMergeRoot(runID)
	if dir == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && st.IsDir()
}

func (s *Service) removeRepoTargetedMergeRoot(runID string) {
	if dir := s.repoTargetedMergeRoot(runID); dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// mergeGitAuthArgs carries the forge token as a per-invocation header
// instead of embedding it in the remote URL: this clone can outlive the
// request (conflict resolution), so no credential may rest in its
// .git/config. `oauth2` as basic-auth username is accepted by GitLab,
// GitHub and Forgejo alike.
func mergeGitAuthArgs(token string) []string {
	if token == "" {
		return nil
	}
	b := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	return []string{"-c", "http.extraHeader=AUTHORIZATION: Basic " + b}
}

// runMergeGit executes one git command for the merge clone, with the
// forge header injected and the token redacted from any error output.
//
// NoAutoMaintenance keeps the command's lifetime whole: fetch, merge and
// commit each end by DETACHING a `git maintenance run --auto` that keeps
// writing under `.git/objects`, and this clone is removed the moment the
// merge lands (removeRepoTargetedMergeRoot) — a partially removed tree that
// still has a `.git` reads as a materialised clone to the next request.
func runMergeGit(ctx context.Context, dir, token string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, mergeGitTimeout)
	defer cancel()
	full := gitlib.NoAutoMaintenance(append(mergeGitAuthArgs(token), args...)...)
	cmd := exec.CommandContext(ctx, "git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	// SanitizeEnv drops GIT_DIR / GIT_COMMON_DIR / GIT_INDEX_FILE so this
	// clone's cmd.Dir is the repository, not an inherited redirection.
	cmd.Env = append(gitlib.SanitizeEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	text := string(out)
	if token != "" {
		text = strings.ReplaceAll(text, token, "***")
	}
	if err != nil {
		return text, fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(text))
	}
	return text, nil
}

// ensureRepoTargetedMergeRoot materialises (or reuses) the merge clone for
// a repo-targeted run: a checkout of the merge target branch with the
// run's storage branch fetched under its own local name, so the regular
// merge pipeline finds both exactly where a local repo would have them.
// mergeInto picks the target branch; empty falls back to the launch ref
// (r.RepoSHA), which the publisher persisted at launch.
func (s *Service) ensureRepoTargetedMergeRoot(ctx context.Context, r *store.Run, token, mergeInto string) (string, error) {
	dir := s.repoTargetedMergeRoot(r.ID)
	if dir == "" {
		return "", fmt.Errorf("runview: no directory to host the merge clone for run %s", r.ID)
	}
	if s.hasRepoTargetedMergeRoot(r.ID) {
		return dir, nil
	}
	target := strings.TrimSpace(mergeInto)
	if target == "" {
		target = strings.TrimSpace(r.RepoSHA)
	}
	if target == "" {
		return "", fmt.Errorf("run %q records no launch ref and no merge target was given — pass one explicitly (runs merge --into <branch>)", r.ID)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", fmt.Errorf("prepare merge clone dir: %w", err)
	}
	// Same SSRF hardening as the runner's clone: a redirect off the
	// validated forge host must not be followed.
	if _, err := runMergeGit(ctx, "", token,
		"-c", "http.followRedirects=false",
		"clone", "--no-tags", "--quiet", "--branch", target, r.RepoURL, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("clone %s at %q for the merge: %w", r.RepoURL, target, err)
	}
	if _, err := runMergeGit(ctx, dir, token,
		"-c", "http.followRedirects=false",
		"fetch", "--no-tags", "--quiet", "origin",
		"+refs/heads/"+r.FinalBranch+":refs/heads/"+r.FinalBranch); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("fetch storage branch %s: %w", r.FinalBranch, err)
	}
	// The clone has no gitconfig; the merge/squash commit needs an
	// identity. Neutral bot identity, local to this clone.
	if _, err := runMergeGit(ctx, dir, "", "config", "user.name", "iterion-merge[bot]"); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if _, err := runMergeGit(ctx, dir, "", "config", "user.email", "iterion-merge@bot.iterion.invalid"); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// pushRepoTargetedMerge publishes the advanced target branch back to the
// forge. No force: the target moved under us means someone else pushed
// since the clone, and their work must win a manual look, not a rewrite.
func (s *Service) pushRepoTargetedMerge(ctx context.Context, root, token, target string) error {
	if target == "" {
		return fmt.Errorf("push: empty merge target")
	}
	if _, err := runMergeGit(ctx, root, token,
		"-c", "http.followRedirects=false",
		"push", "--quiet", "origin", "refs/heads/"+target+":refs/heads/"+target); err != nil {
		return err
	}
	return nil
}
