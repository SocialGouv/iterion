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
	"strconv"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
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
func (r *Runner) recordRunGitMeta(ctx context.Context, msg *queue.RunMessage, workDir, base string, integ runtime.WorkspaceIntegrity) {
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
	// Same rationale when the workspace is an export-based sandbox COPY
	// whose pod-side truth doesn't confirm "no commits": an empty snapshot
	// here may just mean the export lost them (run 01a02a4b recorded a
	// confident zero for a run whose gate cited its commit hashes). Skip —
	// bankRepoWorkspace raises the loud error; this path only refuses to
	// serve the lie.
	if integ.Applicable && (integ.CaptureErr != "" || integ.PodHead != base) {
		if head, herr := gitlib.RevParseHead(workDir); herr == nil && head == base {
			r.cfg.Logger.Warn("runner: run %s: NOT recording a git snapshot — workspace still at the baseline while the sandbox-side HEAD is %s (capture error: %s)",
				msg.RunID, strutil.FirstNonBlank(integ.PodHead, "unknown"), strutil.FirstNonBlank(integ.CaptureErr, "none"))
			return
		}
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
	// An empty range never overwrites a recorded one. A resumed attempt runs
	// on a FRESH pod that clones at RepoSHA, so its base is that clone's HEAD
	// and its range is empty until it commits something of its own — and the
	// save is a full replace. Writing it would erase the earlier attempt's
	// real commits, which is how run 019f8e08 lost 40 of them. An empty
	// snapshot may only ever CREATE the first record, never replace one.
	if len(meta.Commits) == 0 {
		if prev, perr := gs.LoadRunGitMeta(idCtx, msg.RunID); perr == nil && prev != nil && len(prev.Commits) > 0 {
			r.cfg.Logger.Info("runner: run %s: keeping the recorded git snapshot (%d commit(s)) — this attempt produced none",
				msg.RunID, len(prev.Commits))
			return
		}
	}
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

// reExecutionReason names why this claim is a re-execution — so the clone
// about to replace the workspace is discarding an earlier node's uncommitted
// output — or returns "" for a genuine first attempt, which has nothing to
// lose.
//
// The two reasons are distinct facts and the marker carries whichever applies.
// `msg.Resume` is the ordinary resume publish; it does not cover every
// re-execution, since a JetStream redelivery of a run still marked `running` —
// a pod that died inside the orphan sweeper's window — re-clones with Resume
// nil. The checkpoint is the fact that does not depend on how the delivery was
// shaped: it exists if and only if a node boundary was already crossed.
func (r *Runner) reExecutionReason(ctx context.Context, msg *queue.RunMessage) string {
	if msg.Resume != nil {
		return "resume"
	}
	run, err := r.cfg.Store.LoadRun(ctx, msg.RunID)
	if err != nil {
		// Say so rather than let "could not tell" read as "first attempt":
		// silence here is the exact failure this marker exists to end.
		r.cfg.Logger.Warn("runner: run %s: could not read the run to tell a first claim from a re-execution (%v) — not recording a workspace reset", msg.RunID, err)
		return ""
	}
	if run != nil && run.Checkpoint != nil {
		return "redelivery"
	}
	return ""
}

// recordWorkspaceReset puts the fresh-clone fact on a re-executing run's
// timeline.
//
// It is observational, so a store that refuses the append must not sink the
// run — but it is NOT decorative: it is the only trace that a node's
// uncommitted output was discarded between two attempts, and a bot whose
// contract is "node A edits, node B commits" reads exactly the same on both
// sides of it unless something says otherwise. The pod log therefore carries
// it too, and carries it FIRST: the append is precisely what fails when the
// timeline is going to be missing the fact.
func (r *Runner) recordWorkspaceReset(ctx context.Context, msg *queue.RunMessage, reason string) {
	r.cfg.Logger.Warn("runner: run %s re-executing (%s) on a FRESH clone of %s — any file an earlier node edited but did not commit is gone (node outputs are restored from the checkpoint, the working tree is not)",
		msg.RunID, reason, strutil.FirstNonBlank(msg.RepoSHA, msg.RepoURL))
	if _, err := r.cfg.Store.AppendEvent(ctx, msg.RunID, store.Event{
		Type: store.EventRunWorkspaceReset,
		Data: map[string]any{
			"reason":   reason,
			"repo_url": msg.RepoURL,
			"repo_sha": msg.RepoSHA,
		},
	}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit run_workspace_reset: %v", msg.RunID, err)
	}
}

// prepareRepoWorkspace clones the run's RepoURL@RepoSHA into a fresh per-run
// directory and returns its path. For a private repo it authenticates the
// HTTPS clone with the bound forge token (forge_token / gitlab_token /
// github_token from the sealed bundle). The default branch is cloned first so
// the review base (typically `main`) is present, then the run's ref is fetched
// and checked out so merge-base diffs resolve.
//
// A re-execution then restores what the run's earlier attempt banked or
// parked on the forge (restoreBankedChain). The second return value is the
// baseline the run's work is measured against: the restored chain's own base
// when a chain was restored, "" when the clone's HEAD is the baseline.
func (r *Runner) prepareRepoWorkspace(ctx context.Context, msg *queue.RunMessage) (string, string, error) {
	// RepoURL/RepoSHA arrive from a webhook payload (the generic webhook
	// body is fully attacker-controlled) and flow into git below
	// unmodified. Validate the transport + ref shape BEFORE touching the
	// filesystem or spawning git so a remote-helper URL (`ext::sh -c …`)
	// or a flag-shaped ref (`--upload-pack=…`) can never reach the
	// subprocess. This is the runner's flag/transport-injection boundary,
	// mirroring the bot-install path.
	pinnedIP, err := validateRepoTarget(ctx, msg.RepoURL, msg.RepoSHA)
	if err != nil {
		return "", "", err
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
			return "", "", fmt.Errorf("runner: %w", perr)
		}
		defer stopProxy()
		gitEnv = cloneGuardEnv(endpoint)
	}
	dir := filepath.Join(r.cfg.WorkDir, "repos", msg.RunID)
	if err := os.RemoveAll(dir); err != nil {
		return "", "", fmt.Errorf("clean repo dir: %w", err)
	}
	// A re-execution never inherits the previous attempt's tree: executeRun
	// deletes this directory when the run returns, and the next claim is
	// normally another pod anyway. The checkpoint restores node OUTPUTS, so a
	// downstream node keeps reading "the previous node edited these files"
	// against a tree where those edits no longer exist. That divergence used
	// to be entirely silent — record it on the timeline.
	reason := r.reExecutionReason(ctx, msg)
	if reason != "" {
		r.recordWorkspaceReset(ctx, msg, reason)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir repo parent: %w", err)
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
		if tok == "" {
			// The clone ran with NO forge credential in the run's bundle —
			// for a private repo that fails on symptoms that never name this
			// cause ("could not read Username…"). The usual reason: the
			// workflow declares no forge_token secret, so the repo-targeted
			// launch's SecretOverrides had nothing to fill (overrides only
			// populate DECLARED secrets).
			return "", "", fmt.Errorf("%w (clone ran credential-less: no forge_token/gitlab_token/github_token in the run's sealed bundle — a private repo needs the workflow to declare a forge_token secret for the launch override to fill)", err)
		}
		return "", "", err
	}
	if ref := strings.TrimSpace(msg.RepoSHA); ref != "" {
		if err := r.runGitEnv(ctx, dir, tok, gitEnv, "-c", "http.followRedirects=false", "fetch", "--no-tags", "--quiet", "origin", ref); err != nil {
			return "", "", err
		}
		if err := r.runGit(ctx, dir, tok, "checkout", "--quiet", "-B", ref, "FETCH_HEAD"); err != nil {
			return "", "", err
		}
	}
	// A re-execution is not a fresh start: whatever the earlier attempt
	// banked or parked on the forge is this run's own state, and the resumed
	// nodes must find it in the tree, not only in the checkpoint's outputs.
	baseline := ""
	if reason != "" {
		baseline = r.restoreBankedChain(ctx, msg, dir, tok, gitEnv)
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
		return "", "", fmt.Errorf("runner: seed git author name in %s: %w", dir, err)
	}
	if err := r.runGit(ctx, dir, "", "config", "user.email", authorEmail); err != nil {
		return "", "", fmt.Errorf("runner: seed git author email in %s: %w", dir, err)
	}
	if err := r.installGitCredentialStore(ctx, dir, msg.RepoURL, tok); err != nil {
		// Fail the clone rather than proceed: the alternative is a workspace
		// whose only credential is the frozen one in remote.origin.url, which
		// works now and 403s hours later at push time — the exact failure this
		// removes, and the hardest kind to attribute.
		return "", "", fmt.Errorf("runner: wire git credentials for %s: %w", msg.RunID, err)
	}
	seedRunScratchIgnore(dir)
	r.cfg.Logger.Info("runner: cloned %s@%s for run %s", msg.RepoURL, msg.RepoSHA, msg.RunID)
	return dir, baseline, nil
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
	// ValidateCloneSource already requires the scp-like colon to come
	// before any slash and the host to be non-empty; the shared helper
	// enforces the same shape.
	return gitlib.RepoHost(repoURL)
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
	_, err := r.runGitOutEnv(ctx, dir, tok, extraEnv, args...)
	return err
}

// runGitOutEnv is runGitEnv returning the command's combined output —
// for the callers that need to READ git (ls-remote, rev-list), with the
// same timeout, cancellation hardening and token redaction.
func (r *Runner) runGitOutEnv(ctx context.Context, dir, tok string, extraEnv []string, args ...string) (string, error) {
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
	// SanitizeEnv drops the variables that would override which repository,
	// index or object store this runs against — the clone dir is chosen here.
	cmd.Env = append(gitlib.SanitizeEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail, shown := strings.TrimSpace(string(out)), strings.Join(args, " ")
		if tok != "" {
			detail = strings.ReplaceAll(detail, tok, "***")
			shown = strings.ReplaceAll(shown, tok, "***")
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && gitOpTimeout > 0 {
			return "", fmt.Errorf("git %s: timed out after %s (ITERION_RUNNER_GIT_TIMEOUT bounds each git op): %w: %s", shown, gitOpTimeout, err, detail)
		}
		return "", fmt.Errorf("git %s: %w: %s", shown, err, detail)
	}
	return string(out), nil
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

// renderGitCredentialLine renders the single credential-store line
// (https://oauth2:<token>@host) for a repo's canonical host. Shared by
// the host file write below and the sandbox workspace write-through
// (sandbox_registry.go) so both locations always carry the same shape.
func renderGitCredentialLine(repoURL, token string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("credential store: unusable repo URL %q", repoURL)
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, User: url.UserPassword("oauth2", token)}).String() + "\n", nil
}

// writeGitCredentials renders one credential-store line
// (https://oauth2:<token>@host) and replaces the file atomically, so a
// concurrent git read never sees a torn line.
func writeGitCredentials(path, repoURL, token string) error {
	line, err := renderGitCredentialLine(repoURL, token)
	if err != nil {
		return err
	}
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

// bankRepoWorkspace pushes a repo-targeted run's work to the forge as a
// per-run storage branch and records FinalCommit/FinalBranch, so
// `runs merge` has something to merge. The worktree-finalization path
// that normally banks a run never fires on this path ("store has no
// filesystem root — worktree isolation skipped"), which used to leave
// the run's commits only in this pod's soon-wiped clone.
//
// Called for every bankable outcome (bankableStatus): a finished run,
// and the deaths — budget_exceeded, failed — whose successor would
// otherwise restart from the base commit with the work stranded in the
// git-meta snapshot. The push is force-to-own-branch and the branch name
// is per-run, so a later attempt that ends better simply overwrites —
// and only a better one does: pushBank's guard refuses a strictly poorer
// chain, and leases the ref state it compared against.
//
// A push failure never changes the run's outcome — but it is never
// silent either: on the FIRST bank the cause lands in FinalBranchError
// while FinalBranch stays empty, so merge keeps refusing with the truth
// instead of a bare "nothing to merge". Once an earlier attempt HAS
// banked, that pair is valid and mergeable, so a later attempt's refusal
// is recorded on the run's timeline (run_bank_refused) instead — never
// by overwriting the pair or the field whose meaning is "this commit has
// no branch guarding it".
func (r *Runner) bankRepoWorkspace(ctx context.Context, msg *queue.RunMessage, workDir, base string, integ runtime.WorkspaceIntegrity, finalStatus string) {
	head, refusal := r.resolveBankHead(ctx, msg.RunID, workDir, base, integ)
	if refusal != "" {
		r.recordBankFailure(msg, refusal)
		return
	}
	if head == "" {
		return
	}
	r.pushBank(ctx, msg, workDir, head, finalStatus)
}

// resolveBankHead applies the export-integrity oracle to the host clone
// and answers which commit a bank may push. The one head-resolution
// authority for BOTH bank shapes (storage branch and attempt ref), so
// the two cannot drift on what "the run's final tree" means. Outcomes:
//   - head != "": bankable (a caveat, when any, is already logged);
//   - head == "" && refusal == "": nothing to bank — a verified no-op,
//     or an unreadable HEAD with no integrity signal (warned, as before);
//   - head == "" && refusal != "": refuse loudly — the CALLER records it
//     (FinalBranchError for a terminal bank, the timeline for an
//     attempt ref), because where the refusal must land differs.
func (r *Runner) resolveBankHead(ctx context.Context, runID, workDir, base string, integ runtime.WorkspaceIntegrity) (string, string) {
	head, headErr := gitlib.RevParseHead(workDir)
	if integ.Applicable {
		// The workspace is a pod-side COPY streamed back at sandbox
		// teardown, and the engine captured the pod's final HEAD just
		// before that export. Verify the copy carries it before drawing
		// any conclusion from the host tree: run 01a02a4b finished
		// converged, its export was even logged ok, yet this function
		// read head==base and concluded "nothing to bank" — a silent
		// total loss of the run's work.
		switch {
		case integ.CaptureErr != "":
			if headErr != nil || (base != "" && head == base) {
				return "", fmt.Sprintf(
					"bank refused: pod-side HEAD unknown (%s) and the exported workspace shows no new work (HEAD %s, baseline %s) — cannot tell 'no commits' from 'the export lost the work'",
					integ.CaptureErr, strutil.FirstNonBlank(head, "unreadable"), strutil.FirstNonBlank(base, "unknown"))
			}
			// The host tree does carry new commits; completeness is
			// unverifiable but preserving the visible work wins.
			r.cfg.Logger.Warn("runner: run %s: banking WITHOUT pod-side verification (capture failed: %s) — the branch may be missing commits that never left the pod", runID, integ.CaptureErr)
		case headErr != nil || head != integ.PodHead:
			// The export can deliver every OBJECT yet leave the clone's
			// ref system stale: tar cannot delete, so when a pod-side
			// `git gc`/`pack-refs --all --prune` moved a ref into
			// packed-refs, a leftover host loose ref shadows it (git
			// resolves loose before packed) and HEAD reads pre-run.
			// When the pod's final commit itself made it across, bank
			// THAT exact commit by SHA — it is the tree the run
			// finished on. Otherwise refuse loudly.
			if headErr == nil && r.hostHasCommit(ctx, workDir, integ.PodHead) {
				r.cfg.Logger.Warn("runner: run %s: exported workspace reads %s but the pod-side HEAD %s IS present host-side (stale ref shadowing) — banking the pod's final commit by SHA", runID, head, integ.PodHead)
				return integ.PodHead, ""
			}
			return "", fmt.Sprintf(
				"bank refused: the run finished at pod-side HEAD %s but the exported workspace reads %s — the export did not deliver the run's final tree",
				integ.PodHead, strutil.FirstNonBlank(head, "unreadable"))
		}
	}
	if headErr != nil {
		r.cfg.Logger.Warn("runner: run %s: bank: read HEAD: %v", runID, headErr)
		return "", ""
	}
	if base != "" && head == base {
		confirmed := ""
		if integ.Applicable {
			confirmed = " (pod-side HEAD confirms it)"
		}
		r.cfg.Logger.Info("runner: run %s: nothing to bank — HEAD is still the clone baseline%s", runID, confirmed)
		return "", ""
	}
	return head, ""
}

// bankAttemptRef parks an attempt's work on a uniquely-named ref —
// iterion/run-<id>-parked-<head12>. Same head-derived shape as
// preserveSupersededChain's archives, but a DISTINCT infix on purpose:
// an archive names the complete chain of a dead attempt (a once-valid
// FinalBranch), a parked ref names the half-done work of a run that may
// still be alive, and a future pruning policy must be able to tell the
// two apart by name alone. Parked when the STORAGE branch must not be
// touched: an
// interrupted delivery (the lease may already belong to another pod,
// and two pods force-pushing one branch is the split-brain the bank
// refusal exists to prevent — but a ref carrying this chain's own head
// in its NAME cannot contest anything), a paused run (recording
// FinalBranch would make a half-done run merge-eligible mid-flight),
// and a bankable death whose run ctx was cancelled for lease loss.
//
// Without it, those attempts' commits exist only in the git-meta
// snapshot, and turning that snapshot back into a branch takes a manual
// replay every time — the same measured cost the death bank closed for
// budget/failure outcomes, still being paid for these.
//
// Deliberately DOC-LESS: no FinalBranch (merge eligibility), no
// FinalCommit, no FinalBranchError (the run is not terminally
// unbanked — it may resume or redeliver). The ref lands on the run's
// timeline as run_bank_attempt, success or failure — never silence.
//
// The run ctx on these paths is typically already cancelled, so the
// push runs on its own detached, bounded context. That is safe here
// precisely because the ref is uncontested: the anti-clobber machinery
// (ls-remote, lease, supersede) exists for a SHARED name and would be
// dead weight on a name derived from the pushed head itself. The push
// is plain (no force): a ref already at this head is a no-op, an
// existing ANCESTOR is fast-forwarded (contained in the new chain —
// nothing lost), and a divergent squatter — a 12-hex prefix collision —
// is refused by git and reported, never overwritten.
func (r *Runner) bankAttemptRef(msg *queue.RunMessage, workDir, base string, integ runtime.WorkspaceIntegrity, cause string) {
	ctx, cancel := attemptBankContext()
	defer cancel()
	head, refusal := r.resolveBankHead(ctx, msg.RunID, workDir, base, integ)
	if refusal != "" {
		r.cfg.Logger.Error("runner: run %s: attempt ref not parked (%s): %s", msg.RunID, cause, refusal)
		r.recordBankAttempt(msg, map[string]any{"cause": cause, "error": refusal})
		return
	}
	if head == "" {
		// A verified no-op stays silent — but an unreadable HEAD is not a
		// no-op, and on THIS path the event is the only durable record.
		// (The storage bank keeps its historical warn-only behaviour: its
		// refusals land on FinalBranchError, a field this path never
		// touches.)
		if _, err := gitlib.RevParseHead(workDir); err != nil {
			r.recordBankAttempt(msg, map[string]any{"cause": cause,
				"error": "workspace HEAD unreadable — nothing parked, the work stays in the git-meta snapshot: " + err.Error()})
		}
		return
	}
	// Doc re-read before the push: an operator cancel can land without
	// ever cancelling THIS pod's ctx (the cancel subscription is
	// best-effort — "continuing without"), and refused work parks
	// nowhere. Unlike pushBank's re-read — which runs AFTER its push and
	// only withholds merge eligibility — this one withholds the push
	// itself, and `cancelled` is a status the product RESUMES (rewind
	// parks live runs there), so the skip is recorded on the timeline,
	// never silent. Bounded read, fail-open on error: the read is a
	// courtesy to the operator, not a gate on preserving work — and not
	// a licence for a wedged store to pin the pod. Its own tight bound:
	// wide enough that a merely SLOW store does not fail the guard open
	// (proven at 7s), tight enough that a wedged store cannot buy its
	// whole window in pre-Nak recovery latency (measured at 30s when
	// this rode the doc pair's bound). Past it, the documented
	// preservation-wins residue applies.
	rctx, rcancel := context.WithTimeout(ctx, parkDocReadTimeout)
	run, lerr := r.cfg.Store.LoadRun(store.WithIdentity(rctx, msg.TenantID, msg.OwnerID), msg.RunID)
	rcancel()
	if lerr == nil && run != nil && run.Status == store.RunStatusCancelled {
		r.cfg.Logger.Warn("runner: run %s: NOT parking (%s) — the run doc reads cancelled; the operator refused the work.", msg.RunID, cause)
		r.recordBankAttempt(msg, map[string]any{"cause": cause, "head": head,
			"error": "not parked — the run doc reads cancelled (the work stays in the git-meta snapshot)"})
		return
	}
	ref := "iterion/run-" + msg.RunID + "-parked-" + shortSHA(head)
	if err := r.runGit(ctx, workDir, "", "push", "origin", head+":refs/heads/"+ref); err != nil {
		r.cfg.Logger.Error("runner: run %s: attempt ref push %s FAILED — the work exists only in this pod's clone and the git-meta snapshot: %v", msg.RunID, ref, err)
		r.recordBankAttempt(msg, map[string]any{"cause": cause, "error": fmt.Sprintf("push %s: %v", ref, err)})
		return
	}
	r.cfg.Logger.Info("runner: run %s: attempt work parked at %s @ %.12s (%s)", msg.RunID, ref, head, cause)
	r.recordBankAttempt(msg, map[string]any{"cause": cause, "ref": ref, "head": head})
}

// attemptBankBudget bounds the park's GIT work end to end; each store
// op rides its own short bound on top (parkStoreOpTimeout for the
// event write — detached on purpose, so the event still lands when the
// push just died on this budget; parkDocReadTimeout for the doc read).
// Deliberately far below bankBudget: the park is ONE push, not the
// bank's four network ops — and on the interrupted path it runs BEFORE
// the Nak that triggers redelivery, so every second spent here is
// added recovery latency for a run another pod should already be
// picking up. A var so tests can exercise the spent-budget divergence.
var attemptBankBudget = 2 * time.Minute

// parkStoreOpTimeout bounds the park's timeline event write (the doc
// courtesy read rides parkDocReadTimeout) — the store is the same
// class of external resource as the forge, on the same pre-Nak path,
// and the mongo client carries no timeout of its own. Same 5s
// teardown-write convention as runtime's run_failure/pause paths.
const parkStoreOpTimeout = 5 * time.Second

// bankDocOpTimeout bounds the storage bank's run-doc pair I/O
// (pushBank's load→save, recordBankFailure's) — load-bearing for merge
// truth, hence far more generous than parkStoreOpTimeout, but bounded
// all the same: the storage bank is PRE-Nak too (a generic `failed` is
// bankable and naks only after banking returns, with the heartbeat
// still refreshing the lease), and a doc write that never returns does
// not serve merge truth — it replaces it with a pod nothing can
// redeliver. A deadline here fails LOUDLY on the existing error paths.
const bankDocOpTimeout = 30 * time.Second

// parkDocReadTimeout bounds the park's doc-cancel courtesy read alone —
// tighter than the doc pair's bound on purpose: this read sits on the
// interrupted pre-Nak path, where a wedged store buys its whole window
// in added recovery latency (measured at 30s when it rode
// bankDocOpTimeout). 10s covers the slow-but-healthy store the guard
// exists for (proven at 7s) with margin.
const parkDocReadTimeout = 10 * time.Second

// bankStoreCtx builds one bounded store context PER DOC OP: detached
// (the run ctx may already be cancelled), identity-carrying, always
// deadlined. Per op, not per pair — a load that eats the shared window
// starves the save, and a save that dies after the push loses the
// merge truth the pair exists to record (proven: 28s load, 3s save
// that would have fit its own bound, branch orphaned on the forge).
func bankStoreCtx(msg *queue.RunMessage) (context.Context, context.CancelFunc) {
	wctx, cancel := context.WithTimeout(context.Background(), bankDocOpTimeout)
	return store.WithIdentity(wctx, msg.TenantID, msg.OwnerID), cancel
}

// attemptBankContext builds the park's detached context — ALWAYS
// deadlined, including when the operator disabled per-op git bounds
// (ITERION_RUNNER_GIT_TIMEOUT<=0). That choice bounds one git op; it is
// not a licence to make a best-effort parking uninterruptible: the
// caller's ctx is already cancelled on these paths, so nothing else can
// ever stop the push, and a forge that accepts the connection and never
// answers would pin the pod forever (proven by probe). Unlike
// bankContext's unbounded arm, this pod has already lost the run — a
// park it cannot finish in the budget is a park it forfeits, loudly.
func attemptBankContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), attemptBankBudget)
}

// recordBankAttempt puts a park outcome on the run's timeline — the
// only durable record of the parked ref, since the run doc is
// deliberately left untouched on every bankAttemptRef path. The write
// rides its OWN detached short bound, not the park budget: the event
// must land precisely when the push just died on that budget, and a
// wedged store must not pin the pod either way.
func (r *Runner) recordBankAttempt(msg *queue.RunMessage, data map[string]any) {
	wctx, cancel := context.WithTimeout(context.Background(), parkStoreOpTimeout)
	defer cancel()
	idCtx := store.WithIdentity(wctx, msg.TenantID, msg.OwnerID)
	if _, err := r.cfg.Store.AppendEvent(idCtx, msg.RunID, store.Event{
		Type: store.EventRunBankAttempt,
		Data: data,
	}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit run_bank_attempt: %v", msg.RunID, err)
	}
}

// hostHasCommit reports whether sha resolves to a commit object present
// in the clone at workDir.
func (r *Runner) hostHasCommit(ctx context.Context, workDir, sha string) bool {
	return r.runGit(ctx, workDir, "", "cat-file", "-e", sha+"^{commit}") == nil
}

// pushBank pushes the given commit as the run's per-run storage branch
// and persists the outcome: FinalCommit + FinalBranch, or a loud
// FinalBranchError when the forge refused the push. head is a resolved
// SHA — the normal path banks the exported HEAD, the ref-shadowing
// recovery banks the pod-side commit directly.
func (r *Runner) pushBank(ctx context.Context, msg *queue.RunMessage, workDir, head, finalStatus string) {
	branch := "iterion/run-" + msg.RunID
	tok := ""
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		tok = strutil.FirstNonBlank(creds.GenericSecret("forge_token"), creds.GenericSecret("gitlab_token"), creds.GenericSecret("github_token"))
	}
	// An earlier attempt of this run may have banked a RICHER chain than
	// this outcome carries: a redelivered failure re-clones from base,
	// and its retry can legitimately end with fewer commits. "A later
	// attempt that ends better simply overwrites" — so a blind force-push
	// here would enforce "later" without "better", orphaning the richer
	// branch. Push when the branch is absent, unchanged, an ancestor of
	// the new head, or at most as long; refuse loudly when this chain is
	// strictly poorer and leave the branch (and the run doc that already
	// points at it) alone.
	oldHead := r.bankedBranchHead(ctx, workDir, tok, branch)
	if oldHead != "" && oldHead != head {
		// A FINISHED outcome is the run's converged final state and
		// supersedes whatever an earlier dead attempt banked, however
		// long that chain was: a resume re-clones at the base and only
		// re-does the remaining nodes, so the finished chain is
		// legitimately SHORTER than the dead one it completes — refusing
		// it would make `runs merge` land the dead attempt's tree over
		// the finished run's. The chain comparison protects dead
		// attempts from poorer dead attempts, nothing else. Supersede is
		// not destruction, though: when the finished chain does not
		// CONTAIN the banked head, that head may be the only copy of the
		// dead attempt's commits, so it is archived first.
		if finalStatus == "finished" {
			r.preserveSupersededChain(ctx, msg, workDir, tok, branch, oldHead, head)
			r.cfg.Logger.Info("runner: run %s: bank: finished outcome supersedes the chain banked at %.12s", msg.RunID, oldHead)
		} else if !r.bankSupersedes(ctx, msg, workDir, tok, branch, oldHead, head) {
			return
		}
	}
	// Push through `origin` so git resolves the credential from the
	// clone's live store (installGitCredentialStore wired
	// `credential.helper store --file=.git/iterion-credentials`, and
	// refreshGitCredentialsLoop keeps that file on the CURRENT token for
	// the whole run). The previous shape injected the claim-time token
	// into the push URL — a GitHub App installation token lives one hour,
	// a paused-and-resumed run can end far later, and the bank push then
	// died on a dead credential while a live one sat in the store. The
	// claim-time token stays only as the redaction key for error output.
	pushErr := r.runGit(ctx, workDir, tok, bankPushArgs(branch, head, oldHead)...)

	// Persist on detached, deadlined ctxs carrying the run's tenant
	// identity (the run ctx may already be cancelled) — bounded PER OP,
	// because this runs pre-Nak with the lease still refreshing, and a
	// load that eats a shared window starves the save. Every exit past
	// this point that leaves the pushed branch unrecorded goes on the
	// timeline: the doc is what `runs merge` trusts, and a branch the
	// doc does not name is invisible everywhere but the pod log.
	lctx, lcancel := bankStoreCtx(msg)
	run, lerr := r.cfg.Store.LoadRun(lctx, msg.RunID)
	lcancel()
	if lerr != nil || run == nil {
		r.cfg.Logger.Error("runner: run %s: bank: load run to record branch: %v", msg.RunID, lerr)
		data := bankExitData(branch, head, "doc_load_failed", pushErr)
		data["error"] = fmt.Sprintf("the doc read died, nothing recorded: %v", lerr)
		r.recordBankRefused(msg, data)
		return
	}
	// Re-read just before the write: an operator cancel that landed while
	// the push was in flight must not become merge-eligible through this
	// SaveRun — the branch may exist on the forge, but the doc is what
	// `runs merge` trusts, and a cancel is the operator refusing the
	// work.
	if run.Status == store.RunStatusCancelled {
		r.cfg.Logger.Warn("runner: run %s: bank: run was cancelled while banking — leaving FinalBranch unset (branch %s pushed but not recorded)", msg.RunID, branch)
		r.recordBankRefused(msg, bankExitData(branch, head, "cancelled_while_banking", pushErr))
		return
	}
	// The three fields must stay mutually consistent ACROSS ATTEMPTS. A
	// bankable death naks, so the redelivered attempt banks into this same
	// doc — and `runs merge` reads FinalBranch and FinalCommit TOGETHER
	// (PerformDeferredMerge takes BranchToMerge + FinalSHA, and
	// BuildSquashMessage resolves FinalCommit in a clone that only fetched
	// the branch).
	if pushErr != nil {
		// This attempt's head is NOT on the forge. When an earlier attempt
		// already banked a valid pair, this failure must not TOUCH it:
		// writing FinalBranchError over it would hide the good branch from
		// the studio (it renders the branch row only when the error field
		// is empty) and break the field's contract ("FinalCommit is set
		// but has no branch guarding it"). The failure goes on the run's
		// timeline instead, and the doc keeps telling the truth about the
		// branch that exists.
		if run.FinalBranch != "" {
			r.cfg.Logger.Error("runner: run %s: bank push %s FAILED — keeping the earlier attempt's banked pair (%s @ %.12s): %v", msg.RunID, branch, run.FinalBranch, run.FinalCommit, pushErr)
			r.recordBankRefused(msg, map[string]any{
				"branch":    branch,
				"kept_head": run.FinalCommit,
				"reason":    "push_failed",
				"error":     pushErr.Error(),
			})
			return
		}
		run.FinalCommit = head
		run.FinalBranchError = fmt.Sprintf("bank push %s: %v", branch, pushErr)
		r.cfg.Logger.Error("runner: run %s: bank push %s FAILED — the work exists only in this pod's clone: %v", msg.RunID, branch, pushErr)
	} else {
		run.FinalCommit = head
		run.FinalBranch = branch
		// A later attempt that banks cleanly clears an earlier attempt's
		// recorded failure: leaving it would make a perfectly banked run
		// report a bank failure forever on the field `runs merge` surfaces.
		run.FinalBranchError = ""
		r.cfg.Logger.Info("runner: run %s banked: %s @ %.12s", msg.RunID, branch, head)
	}
	sctx, scancel := bankStoreCtx(msg)
	defer scancel()
	if serr := r.cfg.Store.SaveRun(sctx, run); serr != nil {
		r.cfg.Logger.Error("runner: run %s: bank: persist FinalBranch: %v", msg.RunID, serr)
		data := bankExitData(branch, head, "doc_save_failed", pushErr)
		data["error"] = serr.Error()
		r.recordBankRefused(msg, data)
	}
}

// bankExitData shapes a post-push doc-I/O exit for the timeline. head
// sits on the forge only when the push SUCCEEDED, so a dead push is
// named on every such exit: without it the event is key-for-key
// identical to the push-succeeded shape, and once the doc write meant
// to carry FinalBranchError has died too, push_error is the cause's
// only durable carrier.
func bankExitData(branch, head, reason string, pushErr error) map[string]any {
	data := map[string]any{"branch": branch, "head": head, "reason": reason}
	if pushErr != nil {
		data["push_error"] = pushErr.Error()
	}
	return data
}

// bankPushArgs builds the bank's push, binding it to the ref state the
// richer-chain guard actually compared against.
//
// bankedBranchHead → bankSupersedes → push is a read-compare-write with
// nothing making it atomic: a sibling attempt that advances the branch
// in between is clobbered anyway, which is precisely the loss the guard
// exists to prevent. --force-with-lease=<ref>:<expect> moves the
// decision into the push itself, so the update lands only if the branch
// is still where we read it. (The lease authorises the non-fast-forward
// update on its own once satisfied — no --force alongside it, which
// would only muddy which of the two is doing the deciding.)
//
// The lease is applied ONLY when oldHead is known. An empty <expect>
// means "the ref must not exist", which would break the very first bank
// and would silently convert an unreadable remote — the documented
// fail-OPEN degradation of bankedBranchHead — into a hard failure.
func bankPushArgs(branch, head, oldHead string) []string {
	refspec := head + ":refs/heads/" + branch
	if oldHead == "" {
		return []string{"push", "--force", "origin", refspec}
	}
	return []string{"push", "--force-with-lease=refs/heads/" + branch + ":" + oldHead, "origin", refspec}
}

// bankedBranchHead reads the forge's current tip of the run's storage
// branch ("" when the branch does not exist, or when the read itself
// fails — an unreadable remote must not block the bank, it degrades to
// the pre-check-less push).
func (r *Runner) bankedBranchHead(ctx context.Context, workDir, tok, branch string) string {
	out, err := r.runGitOutEnv(ctx, workDir, tok, nil, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		r.cfg.Logger.Warn("runner: bank: ls-remote %s: %v — pushing without the prior-attempt check", branch, err)
		return ""
	}
	return parseLsRemoteHead(out, branch)
}

// parseLsRemoteHead picks the sha ls-remote advertised for
// refs/heads/<branch>. It takes the line that actually NAMES the ref,
// not the first token of the output, because runGitOutEnv returns
// COMBINED output: on a network ls-remote any stderr line git emits
// first (a redirect warning, a `remote:` banner) would otherwise become
// the "prior head" — a non-sha token whose fetch then fails, collapsing
// the anti-clobber guard into the blind force-push it exists to
// prevent, exactly when the output is not pristine. Returns "" when the
// branch is not advertised.
func parseLsRemoteHead(out, branch string) string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == "refs/heads/"+branch {
			return f[0]
		}
	}
	return ""
}

// preserveSupersededChain archives the head a finished outcome is about
// to force away, when (and only when) that head is not contained in the
// finished chain. A finished run legitimately supersedes a dead
// attempt's longer chain, but on the documented budget-cap→resume
// recovery the resumed run re-clones at base and re-does the work — it
// can converge without ever carrying the dead attempt's commits, and
// the banked branch may hold their ONLY forge-side copy. The archive
// ref keeps that work reachable at iterion/run-<id>-attempt-<head12>
// without contesting the storage branch, which must point at the
// finished product.
//
// A failure here never blocks the bank: the finished head is what every
// downstream consumer (`runs merge`, the gate, the PR) reads off the
// storage branch, and the dead chain stays recoverable from the run's
// git-meta snapshot — pricier, but not gone. It is never silent either:
// the divergence always lands on the run's timeline as
// run_bank_superseded, carrying either the archive ref or the error.
func (r *Runner) preserveSupersededChain(ctx context.Context, msg *queue.RunMessage, workDir, tok, branch, oldHead, head string) {
	// The banked head came from ls-remote; the finished clone has no
	// reason to carry it. Fetch before any ancestry test or archive push
	// — both need the object locally.
	if err := r.runGit(ctx, workDir, tok, "fetch", "origin", oldHead); err != nil {
		r.cfg.Logger.Error("runner: run %s: bank: cannot fetch superseded head %.12s to archive it — the finished push drops it from %s (still recoverable from the git-meta snapshot): %v", msg.RunID, oldHead, branch, err)
		r.recordBankSuperseded(msg, branch, oldHead, head, "", "fetch superseded head: "+err.Error())
		return
	}
	if r.runGit(ctx, workDir, "", "merge-base", "--is-ancestor", oldHead, head) == nil {
		return // contained in the finished chain — nothing to lose
	}
	archive := branch + "-attempt-" + shortSHA(oldHead)
	if err := r.runGit(ctx, workDir, tok, "push", "origin", oldHead+":refs/heads/"+archive); err != nil {
		r.cfg.Logger.Error("runner: run %s: bank: archive push %s FAILED — the finished push drops %.12s from %s (still recoverable from the git-meta snapshot): %v", msg.RunID, archive, oldHead, branch, err)
		r.recordBankSuperseded(msg, branch, oldHead, head, "", "archive push: "+err.Error())
		return
	}
	r.cfg.Logger.Info("runner: run %s: bank: superseded chain archived at %s @ %.12s", msg.RunID, archive, oldHead)
	r.recordBankSuperseded(msg, branch, oldHead, head, archive, "")
}

// shortSHA is the 12-hex abbreviation used in archive ref names —
// defensive about inputs shorter than an abbreviation, which a forge
// ls-remote never returns but a unit test might.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// recordBankSuperseded puts a finished outcome's takeover of an earlier
// attempt's diverging banked chain on the run's timeline. Exactly one of
// archivedRef / archiveErr is non-empty.
func (r *Runner) recordBankSuperseded(msg *queue.RunMessage, branch, oldHead, newHead, archivedRef, archiveErr string) {
	data := map[string]any{
		"branch":          branch,
		"superseded_head": oldHead,
		"new_head":        newHead,
	}
	if archivedRef != "" {
		data["archived_ref"] = archivedRef
	}
	if archiveErr != "" {
		data["archive_error"] = archiveErr
	}
	// Bounded like every best-effort timeline write on a teardown path
	// (parkStoreOpTimeout): a wedged store must not pin the pod.
	wctx, cancel := context.WithTimeout(context.Background(), parkStoreOpTimeout)
	defer cancel()
	idCtx := store.WithIdentity(wctx, msg.TenantID, msg.OwnerID)
	if _, err := r.cfg.Store.AppendEvent(idCtx, msg.RunID, store.Event{
		Type: store.EventRunBankSuperseded,
		Data: data,
	}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit run_bank_superseded: %v", msg.RunID, err)
	}
}

// bankSupersedes says whether this outcome's head may overwrite the
// branch an earlier attempt banked at oldHead. True when the new head
// contains the old one (a resume that carried the work forward), or when
// its chain is at least as long — later wins only when it is not
// strictly poorer. On the refusal path it RECORDS the loss it prevented,
// on the run's timeline as well as in the pod log; on unverifiable
// ground (fetch failed, no common baseline) it lets the push through —
// the chain comparison is skipped, but the push still carries
// bankPushArgs' lease on the head that was read, so "unverifiable" costs
// the comparison and never the anti-clobber guarantee.
func (r *Runner) bankSupersedes(ctx context.Context, msg *queue.RunMessage, workDir, tok, branch, oldHead, head string) bool {
	runID := msg.RunID
	if err := r.runGit(ctx, workDir, tok, "fetch", "origin", oldHead); err != nil {
		r.cfg.Logger.Warn("runner: run %s: bank: fetch prior banked head %.12s: %v — pushing without the comparison", runID, oldHead, err)
		return true
	}
	if r.runGit(ctx, workDir, "", "merge-base", "--is-ancestor", oldHead, head) == nil {
		return true
	}
	newOut, nerr := r.runGitOutEnv(ctx, workDir, "", nil, "rev-list", "--count", oldHead+".."+head)
	oldOut, oerr := r.runGitOutEnv(ctx, workDir, "", nil, "rev-list", "--count", head+".."+oldHead)
	newCount, ncErr := strconv.Atoi(strings.TrimSpace(newOut))
	oldCount, ocErr := strconv.Atoi(strings.TrimSpace(oldOut))
	if nerr != nil || oerr != nil || ncErr != nil || ocErr != nil {
		r.cfg.Logger.Warn("runner: run %s: bank: cannot compare diverged chains (%v/%v/%v/%v) — pushing, later wins", runID, nerr, oerr, ncErr, ocErr)
		return true
	}
	if newCount >= oldCount {
		return true
	}
	r.cfg.Logger.Warn("runner: run %s: bank REFUSED: an earlier attempt banked a richer chain at %s (%.12s, %d exclusive commits) than this outcome carries (%.12s, %d) — keeping the richer branch",
		runID, branch, oldHead, oldCount, head, newCount)
	r.recordBankRefused(msg, map[string]any{
		"branch":          branch,
		"kept_head":       oldHead,
		"kept_commits":    oldCount,
		"dropped_head":    head,
		"dropped_commits": newCount,
	})
	return false
}

// recordBankRefused puts a refused bank on the RUN's timeline, not only
// in the pod log.
//
// Without it the refusal is invisible to the operator: pushBank returns
// without touching the run doc, so FinalBranch/FinalCommit keep naming a
// valid forge-backed pair produced by a DIFFERENT attempt, and `runs
// merge` merges that attempt's tree — a head this run's own artifacts
// and gate never cite, with nothing anywhere saying the two diverge.
// That silence is what the surrounding code already refuses elsewhere
// ("an integrity refusal is exactly as loud as a failed push, never a
// silent no-op"). FinalBranchError is deliberately NOT the carrier: its
// documented meaning is "FinalCommit has no persistent branch guarding
// it", so writing it here would break the invariant and raise an
// alarming branch failure over a perfectly good branch.
//
// Best-effort, on the identity ctx rather than the run ctx (which may
// already be dead): a store that refuses the append must never change
// the run's outcome.
func (r *Runner) recordBankRefused(msg *queue.RunMessage, data map[string]any) {
	// Bounded like every best-effort timeline write on a teardown path
	// (parkStoreOpTimeout): a wedged store must not pin the pod.
	wctx, cancel := context.WithTimeout(context.Background(), parkStoreOpTimeout)
	defer cancel()
	idCtx := store.WithIdentity(wctx, msg.TenantID, msg.OwnerID)
	if _, err := r.cfg.Store.AppendEvent(idCtx, msg.RunID, store.Event{
		Type: store.EventRunBankRefused,
		Data: data,
	}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit run_bank_refused: %v", msg.RunID, err)
	}
}

// recordBankFailure persists why a run's work could NOT be banked into
// FinalBranchError — the field `runs merge` surfaces — so an integrity
// refusal is exactly as loud as a failed push, never a silent no-op.
//
// It writes that field only when no branch is recorded yet, which is the
// condition its invariant assumes ("FinalCommit is set but no persistent
// branch guards it"). That used to be unconditionally true: the bank ran
// once, on success only. Now a bankable death naks and the redelivered
// attempt banks into the SAME doc, so an attempt that banked cleanly can
// be followed by one the integrity check refuses — a
// kubernetes-export mismatch on the retry, say. Overwriting the field
// there would raise an alarming branch failure over the valid,
// forge-backed, mergeable pair the earlier attempt left, and make this
// function's own "FinalBranch stays empty" false. The refusal is still
// recorded, on the timeline, exactly like a refused chain comparison.
func (r *Runner) recordBankFailure(msg *queue.RunMessage, cause string) {
	r.cfg.Logger.Error("runner: run %s: %s", msg.RunID, cause)
	lctx, lcancel := bankStoreCtx(msg)
	run, lerr := r.cfg.Store.LoadRun(lctx, msg.RunID)
	lcancel()
	if lerr != nil || run == nil {
		// The refusal is often the ONLY information the run leaves (an
		// export mismatch has no branch) — a dead doc read must not
		// erase it; the timeline carries it instead.
		r.cfg.Logger.Error("runner: run %s: bank: load run to record refusal: %v", msg.RunID, lerr)
		r.recordBankRefused(msg, map[string]any{
			"cause":  cause,
			"reason": "doc_load_failed",
			"error":  fmt.Sprintf("%v", lerr),
		})
		return
	}
	if run.FinalBranch != "" {
		r.cfg.Logger.Error("runner: run %s: bank refused for THIS attempt while the doc keeps an earlier attempt's branch %s @ %.12s — not overwriting a valid pair with this refusal: %s",
			msg.RunID, run.FinalBranch, run.FinalCommit, cause)
		r.recordBankRefused(msg, map[string]any{
			"branch":    run.FinalBranch,
			"kept_head": run.FinalCommit,
			"cause":     cause,
		})
		return
	}
	run.FinalBranchError = cause
	sctx, scancel := bankStoreCtx(msg)
	defer scancel()
	if serr := r.cfg.Store.SaveRun(sctx, run); serr != nil {
		r.cfg.Logger.Error("runner: run %s: bank: persist FinalBranchError: %v", msg.RunID, serr)
		r.recordBankRefused(msg, map[string]any{
			"cause":  cause,
			"reason": "doc_save_failed",
			"error":  serr.Error(),
		})
	}
}
