package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
	"github.com/SocialGouv/iterion/pkg/store"
)

// recordRunGitMeta computes the run's commit/file metadata from the clone
// at workDir and persists it into the store, so the server pod can render
// the Commits/Files panels after this runner pod's workspace is gone (the
// cloud path where the live git inspection has no worktree to read). base
// is the clone HEAD captured before the run; workDir is the on-disk clone.
//
// Best-effort throughout: a non-git workDir, an empty range (no commits),
// or a store without the RunGitMetaStore seam all no-op cleanly. Never
// returns an error — the caller has already decided the run's outcome.
func (r *Runner) recordRunGitMeta(ctx context.Context, msg *queue.RunMessage, workDir, base string) {
	gs := store.AsRunGitMetaStore(r.cfg.Store)
	if gs == nil {
		return
	}
	if base == "" {
		// Baseline capture failed earlier (already warned). Persisting a
		// snapshot with no range would serve a CONFIDENT "no commits" for a
		// run that may well have committed — worse than the panel reporting
		// the metadata unavailable. Skip.
		return
	}
	meta, err := store.BuildRunGitMeta(workDir, base)
	if err != nil {
		if !errors.Is(err, gitlib.ErrNotGitRepo) {
			r.cfg.Logger.Warn("runner: run %s: build git meta: %v", msg.RunID, err)
		}
		return
	}
	// Flush on a background ctx carrying the run's tenant identity — NOT the
	// run ctx — so a cancelled/timed-out run still persists its final view
	// (mirrors the run-log writer's flush-ctx rationale).
	idCtx := store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID)
	// Capture per-file diff content (before/after) into the snapshot while the
	// clone still exists, so the server pod can serve /files/diff and
	// /commits/{sha}/diff for this run once the worktree is gone. Bounded:
	// small diffs inline, large ones offloaded to the blob backend, anything
	// past the budget dropped (Truncated). Best-effort — the metadata is the
	// contract; diff content is an enrichment.
	store.PopulateRunDiffs(idCtx, msg.RunID, workDir, meta, store.AsRunDiffBlobStore(r.cfg.Store))
	if err := gs.SaveRunGitMeta(idCtx, msg.RunID, meta); err != nil {
		r.cfg.Logger.Warn("runner: run %s: persist git meta: %v", msg.RunID, err)
	}
}

// prepareRepoWorkspace clones the run's RepoURL@RepoSHA into a fresh per-run
// directory and returns its path. For a private repo it authenticates the
// HTTPS clone with the bound forge token (forge_token / gitlab_token /
// github_token from the sealed bundle). The default branch is cloned first so
// the review base (typically `main`) is present, then the run's ref is fetched
// and checked out so merge-base diffs resolve.
func (r *Runner) prepareRepoWorkspace(ctx context.Context, msg *queue.RunMessage) (string, error) {
	// RepoURL/RepoSHA arrive from a webhook payload (the generic webhook
	// body is fully attacker-controlled) and flow into git below
	// unmodified. Validate the transport + ref shape BEFORE touching the
	// filesystem or spawning git so a remote-helper URL (`ext::sh -c …`)
	// or a flag-shaped ref (`--upload-pack=…`) can never reach the
	// subprocess. This is the runner's flag/transport-injection boundary,
	// mirroring the bot-install path.
	pinnedIP, err := validateRepoTarget(ctx, msg.RepoURL, msg.RepoSHA)
	if err != nil {
		return "", err
	}
	// SSRF connect-time hardening for the two TOCTOU vectors validateRepoTarget
	// alone can't close (it resolves but git re-resolves at connect time):
	//   (a) DNS rebinding — the clone-guard CONNECT proxy below is the
	//       enforcing layer: git dials through a loopback proxy that
	//       re-resolves the CONNECT host through the same SSRF guard and dials
	//       ONLY the validated IP, so it holds on non-root pods too. The
	//       /etc/hosts pin stays as belt-and-braces (it also covers ssh
	//       remotes) but is best-effort: on a non-root runner the file is
	//       kubelet-owned and unwritable — expected, logged once at info.
	//   (b) HTTP 302 → internal — disabled per git invocation below
	//       (http.followRedirects=false), and the proxy's single-host
	//       allowlist refuses any off-host CONNECT regardless.
	// The pod-level egress NetworkPolicy (block RFC1918/metadata) stays as
	// infra defence-in-depth on top.
	host, hostErr := extractRepoHost(msg.RepoURL)
	if hostErr == nil && pinnedIP != nil {
		if restore, perr := pinHostInHostsFile(runnerHostsFile, host, pinnedIP); perr == nil {
			defer restore()
		} else if pinUnavailable(perr) {
			// Expected & permanent on a non-root runner: /etc/hosts is a
			// kubelet-managed bind-mount owned by root, so the pin can never
			// land here. Log ONCE at info — the clone-guard proxy is the
			// connect-time control.
			r.ssrfPinUnavailableOnce.Do(func() {
				r.cfg.Logger.Info("runner: SSRF IP-pin unavailable on this runner: %s not writable (non-root); the clone-guard proxy is the connect-time control (%v)", runnerHostsFile, perr)
			})
		} else {
			// Unexpected: writable file but the write still failed. Keep warning per-clone.
			r.cfg.Logger.Warn("runner: SSRF IP-pin skipped for %s→%s (%v); the clone-guard proxy is the connect-time control", host, pinnedIP, perr)
		}
	}
	// git honours HTTPS_PROXY for http(s) transports only; ssh remotes keep
	// the pre-check + hosts pin + pod egress policy as their guard.
	var gitEnv []string
	if hostErr == nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(msg.RepoURL)), "https://") {
		endpoint, stopProxy, perr := startCloneGuardProxy(host, !cloneAllowPrivate())
		if perr != nil {
			return "", fmt.Errorf("runner: %w", perr)
		}
		defer stopProxy()
		gitEnv = cloneGuardEnv(endpoint)
	}
	dir := filepath.Join(r.cfg.WorkDir, "repos", msg.RunID)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clean repo dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", fmt.Errorf("mkdir repo parent: %w", err)
	}

	cloneURL, tok, appBotLogin := msg.RepoURL, "", ""
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		tok = strutil.FirstNonBlank(creds.GenericSecret("forge_token"), creds.GenericSecret("gitlab_token"), creds.GenericSecret("github_token"))
		appBotLogin = creds.ForgeAppBotLogin
		if tok != "" {
			cloneURL = injectGitToken(msg.RepoURL, tok)
		}
	}

	// -c http.followRedirects=false closes SSRF vector (b): a 302 from the
	// validated canonical https host to an internal address must not be
	// auto-followed by git.
	if err := r.runGitEnv(ctx, "", tok, gitEnv, "-c", "http.followRedirects=false", "clone", "--no-tags", "--quiet", cloneURL, dir); err != nil {
		return "", err
	}
	if ref := strings.TrimSpace(msg.RepoSHA); ref != "" {
		if err := r.runGitEnv(ctx, dir, tok, gitEnv, "-c", "http.followRedirects=false", "fetch", "--no-tags", "--quiet", "origin", ref); err != nil {
			return "", err
		}
		if err := r.runGit(ctx, dir, tok, "checkout", "--quiet", "-B", ref, "FETCH_HEAD"); err != nil {
			return "", err
		}
	}
	// Cloud sandboxes have no ~/.gitconfig (the host bind-mount is dropped on
	// kubernetes and the runner pod has none of its own), so seed an
	// author/committer in the clone's LOCAL config. It travels into the sandbox
	// with .git, so commit-producing bots (feature-dev's commit_changes, willy,
	// billy, docs-refresh, …) don't fail "Author identity unknown".
	//
	// Prefer the identity that OWNS the push token (resolved from the forge) so
	// a pushed commit is attributed to the real pusher, not a stray account
	// sharing the fallback email. Falls back to a neutral bot identity (never a
	// real person's) when there's no token or resolution fails. Overridable via
	// ITERION_GIT_AUTHOR_NAME / ITERION_GIT_AUTHOR_EMAIL.
	authorName, authorEmail := gitAuthorName(), gitAuthorEmail()
	if tok != "" {
		// A github_app connection's forge_token is an installation token that
		// can't `GET /user`; the publisher threads the App bot login so we
		// resolve its canonical committer via `GET /users/<login>` instead.
		if appBotLogin != "" {
			if n, e, ok := resolveAppBotCommitterIdentity(ctx, msg.RepoURL, appBotLogin, tok); ok {
				authorName, authorEmail = n, e
			}
		} else if n, e, ok := resolveForgeCommitterIdentity(ctx, msg.RepoURL, tok); ok {
			authorName, authorEmail = n, e
		}
	}
	if err := r.runGit(ctx, dir, "", "config", "user.name", authorName); err != nil {
		return "", fmt.Errorf("runner: seed git author name in %s: %w", dir, err)
	}
	if err := r.runGit(ctx, dir, "", "config", "user.email", authorEmail); err != nil {
		return "", fmt.Errorf("runner: seed git author email in %s: %w", dir, err)
	}
	if err := r.installGitCredentialStore(ctx, dir, msg.RepoURL, tok); err != nil {
		// Fail the clone rather than proceed: the alternative is a workspace
		// whose only credential is the frozen one in remote.origin.url, which
		// works now and 403s hours later at push time — the exact failure this
		// removes, and the hardest kind to attribute.
		return "", fmt.Errorf("runner: wire git credentials for %s: %w", msg.RunID, err)
	}
	seedRunScratchIgnore(dir)
	r.cfg.Logger.Info("runner: cloned %s@%s for run %s", msg.RepoURL, msg.RepoSHA, msg.RunID)
	return dir, nil
}

// gitAuthorName / gitAuthorEmail are the identity seeded into a cloud clone's
// local git config so an in-sandbox `git commit` has an author even though no
// ~/.gitconfig is mounted. Overridable per-deployment.
func gitAuthorName() string {
	if v := strings.TrimSpace(os.Getenv("ITERION_GIT_AUTHOR_NAME")); v != "" {
		return v
	}
	return "iterion-runner[bot]"
}

func gitAuthorEmail() string {
	if v := strings.TrimSpace(os.Getenv("ITERION_GIT_AUTHOR_EMAIL")); v != "" {
		return v
	}
	// A `.invalid` domain (RFC 2606, reserved, never resolvable) guarantees this
	// fallback maps to NO GitHub account — the commit shows the bot name as
	// plain text, never a stray individual. The default was
	// `iterion@users.noreply.github.com`, which GitHub silently attributed to an
	// unrelated real user "iterion". The push-token identity above is the
	// preferred, attributed path; this only fires token-less.
	return "iterion-runner@bot.iterion.invalid"
}

// seedRunScratchIgnore locally excludes iterion's per-run scratch — the
// .claude/ dir (mirrored skills + claude_code's plan.md) — from the cloned
// repo, so a bot's `git add -A` (which stages new files so the reviewers'
// `git diff HEAD` can see them) doesn't drag that scratch into the review
// diff. Writes .git/info/exclude (local, never committed or pushed);
// best-effort, so a read-only or unusual .git layout simply no-ops.
func seedRunScratchIgnore(dir string) {
	p := filepath.Join(dir, ".git", "info", "exclude")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# iterion per-run scratch (mirrored skills + plan) — not part of the change\n.claude/\n")
}

// validateRepoTarget gates the webhook-sourced clone URL and ref before
// they reach git. It rejects remote-helper transports (`ext::`, `file://`)
// via ValidateCloneSource and flag-shaped refs (leading `-`) via
// ValidateBranchName — the two ways an attacker-controlled RepoURL/RepoSHA
// could turn `git clone`/`git fetch` into arbitrary command execution.
// An empty ref is allowed (the caller only fetches when RepoSHA is non-blank).
//
// After the transport/ref shape passes, the URL's host is run through the
// shared SSRF guard (httpdial.ResolvePublicHost) so a holder of a per-org
// `iwh_` webhook token cannot point the runner at an internal address
// (loopback, RFC1918/ULA, link-local, cloud metadata, cluster aliases) to
// probe the cloud network. Mirrors the completion-webhook guard in pkg/notify.
// On-prem deployments with internal forges set
// ITERION_RUNNER_CLONE_ALLOW_PRIVATE=1 to relax the strict mode.
func validateRepoTarget(ctx context.Context, repoURL, repoSHA string) (net.IP, error) {
	if err := gitlib.ValidateCloneSource(repoURL); err != nil {
		return nil, fmt.Errorf("runner: reject repo url: %w", err)
	}
	if ref := strings.TrimSpace(repoSHA); ref != "" {
		if err := gitlib.ValidateBranchName(ref); err != nil {
			return nil, fmt.Errorf("runner: reject repo ref: %w", err)
		}
	}
	host, err := extractRepoHost(repoURL)
	if err != nil {
		return nil, fmt.Errorf("runner: reject repo url: %w", err)
	}
	allowPrivate := cloneAllowPrivate()
	// First line of defence: refuse non-public hosts before any subprocess
	// spawns, and feed the resolved IP to the /etc/hosts pin. On its own this
	// would be TOCTOU-incomplete (git re-resolves the hostname at connect
	// time); the enforcing layer is the clone-guard CONNECT proxy
	// (startCloneGuardProxy) prepareRepoWorkspace routes https git through,
	// which re-validates and pins the resolved IP at the moment of the dial.
	// ssh remotes are not proxied — for them this pre-check, the hosts pin and
	// the pod egress policy remain the guard.
	ip, err := httpdial.ResolvePublicHost(ctx, host, !allowPrivate)
	if err != nil {
		return nil, fmt.Errorf("runner: repo host %q is not a public address (set ITERION_RUNNER_CLONE_ALLOW_PRIVATE=1 to allow internal forges): %w", host, err)
	}
	return ip, nil
}

// extractRepoHost pulls the host out of a clone URL in the shapes
// ValidateCloneSource permits: `https://host[:port]/...`, `ssh://[user@]host[:port]/...`,
// and scp-like `[user@]host:path`. Returns an error when the host can't be
// determined (defence in depth — ValidateCloneSource has already rejected
// hostless and unsupported-transport forms above).
func extractRepoHost(repoURL string) (string, error) {
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
	// scp-like: `[user@]host:path`. ValidateCloneSource already requires
	// the colon to come before any slash and the host to be non-empty.
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

// gitOpTimeout bounds a single runner-side git subprocess. Without it a
// clone/fetch against a wedged remote hangs on the run ctx alone — and a
// run launched without --timeout has NO deadline, so the git subprocess
// pins the runner pod (one in-flight run each) indefinitely while the heartbeat
// keeps the lease alive (the heartbeat proves the pod lives, not that
// the clone progresses). GIT_TERMINAL_PROMPT=0 covers the credential
// prompt, not a stalled TCP transfer. Override with
// ITERION_RUNNER_GIT_TIMEOUT (a Go duration; <= 0 disables).
var gitOpTimeout = defaultGitOpTimeout()

func defaultGitOpTimeout() time.Duration {
	if v := os.Getenv("ITERION_RUNNER_GIT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d // <= 0 disables the bound
		}
	}
	return 15 * time.Minute
}

// runGit runs a git subprocess, redacting tok from any error output so an
// authed clone URL never leaks into logs. Each invocation is bounded by
// gitOpTimeout on top of the caller's ctx.
func (r *Runner) runGit(ctx context.Context, dir, tok string, args ...string) error {
	return r.runGitEnv(ctx, dir, tok, nil, args...)
}

// runGitEnv is runGit with extra environment entries appended after the
// baseline (later entries win) — network git ops use it to route through the
// clone-guard proxy via HTTPS_PROXY.
func (r *Runner) runGitEnv(ctx context.Context, dir, tok string, extraEnv []string, args ...string) error {
	if gitOpTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, gitOpTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Cancellation must reach git's helper processes (git-remote-https
	// inherits our output pipes — killing only the parent leaves
	// CombinedOutput blocked on the helper's copy), and WaitDelay is the
	// final unblock if a helper still holds them after the group kill.
	hardenGitCancel(cmd)
	cmd.WaitDelay = 10 * time.Second
	// Never prompt for credentials (fail fast instead of hanging), and ignore
	// any host-level git config in the runner image.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail, shown := strings.TrimSpace(string(out)), strings.Join(args, " ")
		if tok != "" {
			detail = strings.ReplaceAll(detail, tok, "***")
			shown = strings.ReplaceAll(shown, tok, "***")
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && gitOpTimeout > 0 {
			return fmt.Errorf("git %s: timed out after %s (ITERION_RUNNER_GIT_TIMEOUT bounds each git op): %w: %s", shown, gitOpTimeout, err, detail)
		}
		return fmt.Errorf("git %s: %w: %s", shown, err, detail)
	}
	return nil
}

// gitCredentialFile is where the clone's live forge token lives, in git's
// credential-store format. It sits under .git/ (not the worktree) so a bot's
// `git add -A` can never stage it.
const gitCredentialFile = "iterion-credentials"

// installGitCredentialStore detaches the clone from the token it was cloned
// with and points git at a credential FILE instead.
//
// The clone URL has to carry the token for the initial fetch to authenticate,
// but git persists remote.origin.url verbatim — which freezes that credential
// into .git/config. A GitHub App installation token lives ONE HOUR, while an
// app-building run takes several, so the agent's `git push` at the end of the
// run would authenticate with a long-dead token and fail 403. Writing the
// credential to a file that the mid-run refresher rewrites means every later
// git operation re-reads a LIVE token. It also keeps the secret out of
// .git/config, where the agent (and anything that dumps config) could read it.
func (r *Runner) installGitCredentialStore(ctx context.Context, dir, repoURL, token string) error {
	if token == "" {
		return nil
	}
	if err := r.runGit(ctx, dir, token, "remote", "set-url", "origin", repoURL); err != nil {
		return fmt.Errorf("clean token out of the origin URL: %w", err)
	}
	path := filepath.Join(dir, ".git", gitCredentialFile)
	if err := writeGitCredentials(path, repoURL, token); err != nil {
		return err
	}
	// --file keeps the store local to this run; git's default would be the
	// pod-global ~/.git-credentials, which a later run could read.
	if err := r.runGit(ctx, dir, "", "config", "credential.helper", "store --file="+path); err != nil {
		return fmt.Errorf("configure credential helper: %w", err)
	}
	return nil
}

// writeGitCredentials renders one credential-store line
// (https://oauth2:<token>@host) and replaces the file atomically, so a
// concurrent git read never sees a torn line.
func writeGitCredentials(path, repoURL, token string) error {
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("credential store: unusable repo URL %q", repoURL)
	}
	line := (&url.URL{Scheme: u.Scheme, Host: u.Host, User: url.UserPassword("oauth2", token)}).String() + "\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(line), 0o600); err != nil {
		return fmt.Errorf("credential store: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("credential store: replace: %w", err)
	}
	return nil
}

// injectGitToken rewrites an https clone URL to carry an oauth2 token in its
// userinfo (works for GitLab project/personal access tokens and GitHub PATs).
func injectGitToken(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return rawURL
	}
	u.User = url.UserPassword("oauth2", token)
	return u.String()
}
