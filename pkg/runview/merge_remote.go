package runview

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
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
// no local store dir, so it falls back to a per-run private temp dir — the
// clone is re-creatable from the forge at any time, so losing it to a pod
// restart only costs a re-clone. The temp fallback is os.MkdirTemp —
// 0700, unpredictable, never a pre-existing path — cached per run for the
// service's lifetime so the clone materialises once and the later merge
// attempts reuse it. The fixed `$TMPDIR/iterion-merges/<runID>` it
// replaced was ADOPTABLE: a local attacker pre-planting the path (a
// hostile `.git` among other shapes) had the run merge inside their clone.
func (s *Service) repoTargetedMergeRoot(runID string) string {
	if runID == "" {
		return ""
	}
	if s.storeDir == "" {
		return s.tempMergeRoot(runID)
	}
	return filepath.Join(s.storeDir, "merges", runID)
}

// tempMergeRoot is the per-run temp fallback of repoTargetedMergeRoot: one
// os.MkdirTemp per run, cached for the service's lifetime. MkdirTemp
// refuses a pre-existing path by construction, so the adoption the fixed
// name allowed is gone; the unpredictable name is what keeps the
// pre-plant from reaching the path at all.
func (s *Service) tempMergeRoot(runID string) string {
	mergeTempSweep.Do(sweepOrphanMergeTemps)
	s.mergeTempsMu.Lock()
	defer s.mergeTempsMu.Unlock()
	if dir, ok := s.mergeTemps[runID]; ok {
		return dir
	}
	dir, err := os.MkdirTemp("", "iterion-merge-")
	if err != nil {
		return ""
	}
	if s.mergeTemps == nil {
		s.mergeTemps = map[string]string{}
	}
	s.mergeTemps[runID] = dir
	return dir
}

// mergeTempSweep fires the orphan sweep once per process, at the first
// temp merge root this process creates: any iterion-merge-* directory
// already in the temp dir predates this process, belongs to no run it
// knows (the mapping is the in-memory cache), and is a repo-sized clone
// nothing else will remove. The storeDir roots are operator-owned and
// never swept.
var mergeTempSweep sync.Once

func sweepOrphanMergeTemps() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "iterion-merge-") {
			_ = os.RemoveAll(filepath.Join(os.TempDir(), e.Name()))
		}
	}
}

// hasRepoTargetedMergeRoot reports whether a materialised merge clone
// already exists for runID.
func (s *Service) hasRepoTargetedMergeRoot(runID string) bool {
	dir := s.knownMergeRoot(runID)
	if dir == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && st.IsDir()
}

// knownMergeRoot is the merge root a PREVIOUS call may have materialised,
// without creating one: the read paths (the existence probe, the removal)
// must not conjure a directory as a side effect.
func (s *Service) knownMergeRoot(runID string) string {
	if runID == "" {
		return ""
	}
	if s.storeDir != "" {
		return filepath.Join(s.storeDir, "merges", runID)
	}
	s.mergeTempsMu.Lock()
	defer s.mergeTempsMu.Unlock()
	return s.mergeTemps[runID]
}

func (s *Service) removeRepoTargetedMergeRoot(runID string) {
	if dir := s.knownMergeRoot(runID); dir != "" {
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
	// fetch/push fork git-remote-https, which inherits the pipes
	// CombinedOutput reads: killing only git leaves the helper holding them
	// and mergeGitTimeout would bound nothing. Same reason the runner's own
	// git wrapper does this.
	proc.TerminateGroupOnCancel(cmd)
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
	// git recreates the target at umask perms when it materialises into
	// the cached path a previous attempt emptied — the clone stays the
	// private directory MkdirTemp made it.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("privatise the merge clone: %w", err)
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
