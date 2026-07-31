package delegate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Handing pi the operator's ChatGPT (Codex) subscription.
//
// pi reaches ~30 providers through an API-key environment variable and
// `openai-codex` through none of them: that provider is OAuth-only, and its
// tokens live in pi's OWN `auth.json`, minted by an interactive `/login`. So a
// host already holding a working Codex credential — the very file iterion
// reads for claw's ChatGPT-forfait path — could not hand it to pi at all, and
// `backend: "pi"` on a ChatGPT plan was simply unreachable.
//
// The bridge is a per-run agent dir: iterion writes the credential pi expects
// into a directory of its own and points `PI_CODING_AGENT_DIR` at it. The
// operator's `~/.pi` is never written to, and the dir is removed when the node
// finishes.
//
// Two consequences are deliberate and worth knowing:
//
//   - Pinning the agent dir HIDES the operator's own `auth.json`, so this only
//     happens for a node that actually asked for `openai-codex`. A node on any
//     other provider keeps pi's native credential breadth, which is half the
//     reason this backend exists.
//   - pi refreshes an EXPIRED token and writes the result to the dir it was
//     given — here, the throwaway one. That keeps `~/.codex` untouched, but it
//     also means the refresh is not shared: if OpenAI rotates the refresh
//     token on use, the copy in `~/.codex/auth.json` can be invalidated and
//     the Codex CLI itself will need to re-login. There is no way to have both
//     without writing into the operator's credential file. Passing the access
//     token's real deadline (see piCodexExpiry) is what keeps this to the
//     exception it should be rather than something every node does.

// piCodexProvider is pi's id for the ChatGPT-subscription provider.
const piCodexProvider = "openai-codex"

// piSeedDirPrefix names the throwaway agent dirs, so a sweep can recognise one
// an interrupted node left behind.
const piSeedDirPrefix = "pi-agent-"

// piNoteCodexFallback explains, once per node, why iterion did not supply the
// credential — silence here would look like the bridge worked.
func piNoteCodexFallback(task Task, logger *iterlog.Logger, reason string) {
	if logger == nil {
		return
	}
	logger.Warn("[%s#%d/%s] node targets %s and %s; leaving pi to resolve its own "+
		"credential (run `pi` then /login, or sign in with `codex login`)",
		task.NodeID, task.Iteration, BackendPi, piCodexProvider, reason)
}

// piOAuthCredential is one entry of pi's auth.json. `type: "oauth"` is the
// discriminator pi's credential store keys on; the field names are pi's, not
// Codex's, which is the whole reason this translation exists.
type piOAuthCredential struct {
	Type    string `json:"type"`
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expires int64  `json:"expires"`
}

// piCodexSeed materialises a pi agent dir carrying the host's Codex OAuth.
//
// Returns the environment overrides to add and a cleanup func. A nil error
// with an empty map means "nothing to do" — the node is not on openai-codex,
// the operator pinned their own agent dir, or no Codex credential exists. Only
// a genuine failure to honour an explicit request returns an error.
func piCodexSeed(ctx context.Context, task Task, logger *iterlog.Logger) (map[string]string, func(), error) {
	noop := func() {}

	provider, _ := piResolveModel(task.Model, task.ProviderHint)
	if provider != piCodexProvider {
		return nil, noop, nil
	}

	// An operator who pinned an agent dir has their own pi configuration,
	// possibly including a real `/login`. Overriding it would silently discard
	// that — including when they pinned pi's OWN variable rather than iterion's,
	// which is the more natural thing to reach for and which the seed would
	// otherwise overwrite through task.ExtraEnv.
	for _, v := range []string{"ITERION_PI_AGENT_DIR", "PI_CODING_AGENT_DIR"} {
		if dir := strings.TrimSpace(os.Getenv(v)); dir != "" {
			piNoteCodexFallback(task, logger, fmt.Sprintf("%s pins its own agent dir", v))
			return nil, noop, nil
		}
	}

	// No usable Codex credential is NOT an error: before this bridge existed,
	// such a node ran off pi's own `/login` store, and failing here would take
	// that working path away — while telling the operator to run `pi` and
	// /login, which this code path never reads. Warn and step aside so pi
	// resolves its own credential, exactly as it used to.
	view, err := piLoadCodexView(ctx)
	switch {
	case err != nil:
		piNoteCodexFallback(task, logger, fmt.Sprintf("no ChatGPT credential could be read (%v)", err))
		return nil, noop, nil
	case !view.IsChatGPTMode():
		piNoteCodexFallback(task, logger, fmt.Sprintf(
			"the Codex credential is not a ChatGPT subscription (auth_mode=%q)", view.AuthMode))
		return nil, noop, nil
	}

	// This IS a subscription, so it answers to the same switch as the Anthropic
	// one: an operator who forbids spending a personal plan through a
	// third-party harness must not have it happen behind their back.
	//
	// Checked only now that a credential is KNOWN to exist, mirroring
	// GuardSubscriptionOAuth. Evaluated earlier it refused on the mere fact that
	// the node targets openai-codex — killing a node running off the operator's
	// own `pi` /login, which this code path never reads and which is their
	// relationship with the vendor, on a statement ("iterion is about to spend
	// your subscription") that was false in that configuration.
	if secrets.ForbidSubscriptionOAuth() {
		return nil, noop, fmt.Errorf(
			"pi backend: node %q targets the %s provider and iterion holds a ChatGPT subscription "+
				"credential for it, but ITERION_FORBID_SUBSCRIPTION_OAUTH is set. Use an "+
				"OPENAI_API_KEY model (openai/…) instead, or unset the switch",
			task.NodeID, piCodexProvider)
	}

	// A root we cannot make safe is not a reason to kill the node: pi's own
	// /login may well work, and that is the path this bridge was added ON TOP
	// of, not in place of. Same rule as a missing credential above.
	root, err := piCodexSeedRoot(task)
	if err != nil {
		piNoteCodexFallback(task, logger, fmt.Sprintf("no safe place to write it (%v)", err))
		return nil, noop, nil
	}
	dir, err := os.MkdirTemp(root, piSeedDirPrefix)
	if err != nil {
		return nil, noop, fmt.Errorf("pi backend: create agent dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	payload, err := json.Marshal(map[string]piOAuthCredential{
		piCodexProvider: {
			Type:    "oauth",
			Access:  view.Tokens.AccessToken,
			Refresh: view.Tokens.RefreshToken,
			Expires: piCodexExpiry(view),
		},
	})
	if err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("pi backend: encode agent credential: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), payload, 0o600); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("pi backend: write agent credential: %w", err)
	}

	if logger != nil {
		logger.Warn("[%s#%d/%s] %s", task.NodeID, task.Iteration, BackendPi,
			secrets.SubscriptionOAuthNotice(secrets.ProviderOpenAI))
	}
	return map[string]string{"PI_CODING_AGENT_DIR": dir}, cleanup, nil
}

// piLoadCodexView reads the Codex credential, preferring the per-run copy the
// cloud runner materialises over the host's ~/.codex (a runner pod has none).
//
// A per-run directory that is announced but unreadable falls back to the host
// rather than ending the search: the two are alternatives, not a chain of
// custody, and preferring one must not cost the other. Locally the ctx copy is
// routinely absent or empty while ~/.codex holds the real login, and treating
// that as "no credential" is indistinguishable, to the operator, from having
// none at all.
func piLoadCodexView(ctx context.Context) (secrets.CodexCredentialsView, error) {
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		if dir := creds.OAuthDir("codex"); dir != "" {
			if view, err := secrets.LoadCodexCredentialsFrom(dir); err == nil && view.IsChatGPTMode() {
				return view, nil
			}
		}
	}
	return secrets.LoadCodexCredentialsFromDisk()
}

// piCodexSeedRoot picks where the throwaway agent dir lives.
//
// It delegates the choice to Task.StateDir, whose second return value is what
// matters here: whether the root is inside the TARGET repository's checkout.
//
// Outside it — the ordinary case, on the host or under a sandbox whose
// host_state mount is active — nothing further is needed. The repo cannot
// pre-populate the directory, so there is no symlink to refuse at any
// component or leaf, no pre-seeded `.gitignore` whose last effective rule might
// re-include our files, and nothing for an agent's `git add -A` to stage. That
// is the point of preferring it: a class of defect removed rather than
// defended.
//
// Inside it — host_state=none, or the kubernetes driver, where the workspace
// bind is the only thing the container can read — every guard below applies,
// because the premise they were written for holds again.
//
// KNOWN EXPOSURE, unchanged by any of this and worth restating. pi's
// `openai-codex` provider is OAuth-only and reads its credential from an agent
// dir; there is no env var to pass it by, unlike the ~30 API-key providers. So
// driving that provider at all means a live access AND refresh token sits on a
// filesystem the agent process can read, and an agent under prompt injection
// has bash. Moving the file out of the worktree stops the repo from REDIRECTING
// or COMMITTING it; it does not stop an agent from reading it. The mitigations
// remain: an OPENAI_API_KEY model rather than `openai-codex/…` for a node
// pointed at an untrusted repository, or ITERION_FORBID_SUBSCRIPTION_OAUTH=1 to
// refuse the bridge outright. Documented in docs/backends.md.
func piCodexSeedRoot(task Task) (string, error) {
	root, inside := task.StateDir(BackendPi)

	// The defences exist because a path inside the target repository's checkout
	// is a path that repo can pre-populate. Outside it — the ordinary sandboxed
	// case now that the shared mount is preferred, plus every store/global case
	// — there is nothing to defend and nothing runs.
	if inside {
		if err := refuseSymlinkedPath(task.WorkDir, root); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create agent dir %s: %w", root, err)
	}
	if inside {
		// The credential must be unstageable: a v2 campaign agent runs `git add
		// -A` before each in-stride commit, and finalizeWorktree fast-forwards
		// the result onto the operator's branch. iterion writes its OWN guard in
		// the seed root rather than trusting one it did not author.
		if err := piWriteIgnoreGuard(root); err != nil {
			return "", err
		}
	}
	return root, nil
}

// lexicallyWithin reports whether target sits under base by PATH ALONE, with no
// filesystem access and no symlink resolution. That is the point: a resolving
// test answers "where does this end up", and the question here is "was this
// written to look like it stays inside the checkout".
func lexicallyWithin(base, target string) bool {
	if base == "" || target == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// relBelow expresses target under base, retrying against the resolved base so a
// workspace spelled through a symlink still yields a walkable path. Returns ""
// when target is not below base at all.
func relBelow(base, target string) (walkBase, rel string) {
	if r, err := filepath.Rel(base, target); err == nil && !strings.HasPrefix(r, "..") {
		return base, r
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", ""
	}
	r, err := filepath.Rel(realBase, target)
	if err != nil || strings.HasPrefix(r, "..") {
		return "", ""
	}
	// Walk from the RESOLVED base, since that is what `r` is relative to.
	return realBase, r
}

// refuseSymlinkedPath rejects target when any component below base is a
// symlink, checked with Lstat so nothing is followed. Components that do not
// exist yet are fine — they are what MkdirAll is about to create.
//
// base itself is not inspected: it is iterion's own worktree path, and a
// symlink there is the engine's business, not the repo's.
func refuseSymlinkedPath(base, target string) error {
	// Containment is the CALLER's decision (Task.StateDir), and it answers on
	// resolved paths — so a target that is genuinely in the tree can still be
	// spelled outside the base lexically, e.g. a workspace reached through a
	// symlink. When that happens there is nothing below `base` for this guard to
	// walk, which is a no-op, NOT a refusal: erroring here silently killed the
	// credential seed (piCodexSeed swallows the error) and the node then died on
	// "No API key found for openai-codex". The leaf symlink is covered
	// separately by piGuardWriteRoot.
	walkBase, rel := relBelow(base, target)
	if rel == "" {
		return nil
	}
	cur := walkBase
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // nothing below it exists either
		}
		if err != nil {
			return fmt.Errorf("inspect %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to seed the pi credential through %s: it is a symlink, "+
				"and the workspace is a checkout of the target repository", cur)
		}
	}
	return nil
}

// refuseNonRegular rejects a path that exists but is not a plain file, checked
// with Lstat so nothing is followed. An absent path is fine — it is what the
// caller is about to create.
//
// Both ignore guards go through this. They write DIFFERENTLY on purpose (the
// seed root is iterion's, so appending is right; `<WorkDir>/.iterion` can be
// the operator's own store, so their file is left untouched) — but the check is
// the same, and hand-rolling it twice is exactly how the same symlink hole came
// to exist at two levels.
func refuseNonRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to use %s: it is not a regular file, and the workspace "+
			"is a checkout of the target repository", path)
	}
	return nil
}

// piWriteIgnoreGuard makes everything under dir unstageable.
//
// It APPENDS. The seed root is `<StoreDir>/pi` off the sandbox, and --store-dir
// is operator-supplied and unconstrained — the dogfood recipe in this repo's own
// docs points it inside the checkout — so the directory is not guaranteed to be
// iterion's to rewrite. Appending buys the same unstageability without
// discarding a file someone else wrote, matching piHideWorkspaceSessionDir's
// rule for the same reason.
func piWriteIgnoreGuard(dir string) error {
	guard := filepath.Join(dir, ".gitignore")
	// The checkout can ship this path as a SYMLINK. MkdirAll above is a no-op on
	// a `.iterion/pi` the repo pre-populated, and neither refuseSymlinkedPath
	// nor piGuardWriteRoot looks INSIDE the seed root — they only walk to it.
	// Two things break if it is followed, and the second is the dangerous one:
	// appending writes "*" into a host file of the repo's choosing, and git
	// REFUSES to read a symlinked .gitignore at all ("unable to access …: Too
	// many levels of symbolic links"), so the guard would fail OPEN and leave
	// the seeded auth.json — a live ChatGPT access AND refresh token —
	// stageable by a campaign agent's `git add -A`.
	//
	// Lstat rather than O_NOFOLLOW: the latter is not portable through `os`,
	// and the residual TOCTOU needs a writer racing us inside the workspace,
	// which is a strictly larger capability than committing a symlink.
	if err := refuseNonRegular(guard); err != nil {
		return err
	}
	existing, err := os.ReadFile(guard) // #nosec G304 — fixed name, verified a regular file just above.
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read ignore guard %s: %w", guard, err)
	}
	// Last match WINS in gitignore, so a `*` anywhere is not proof the tree is
	// ignored — any later `!…` re-includes what it matched. On the sandboxed
	// path this file lives inside the target repository's worktree, which can
	// commit `*\n!auth.json` (or plainly `*\n!*`) and make a naive scan return
	// "already guarded" while the seeded credential stays stageable. Only a `*`
	// that is the LAST effective pattern settles it; otherwise fall through and
	// append one, which then wins over anything above it.
	lines := strings.Split(string(existing), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "*" {
			return nil
		}
		break
	}
	// `*` ignores this file too, so nothing under dir is ever staged.
	next := "*\n"
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		next = "\n" + next
	}
	f, err := os.OpenFile(guard, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // #nosec G304 — fixed name.
	if err != nil {
		return fmt.Errorf("write ignore guard %s: %w", guard, err)
	}
	if _, err := f.WriteString(next); err != nil {
		_ = f.Close()
		return fmt.Errorf("write ignore guard %s: %w", guard, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write ignore guard %s: %w", guard, err)
	}
	return nil
}

// piSeedMaxAge is how old a seed must be before the sweep will touch it.
//
// Generous on purpose: a single campaign node has been measured running 46
// minutes, and deleting a LIVE peer's credential is far worse than leaving an
// abandoned one a few hours longer.
const piSeedMaxAge = 12 * time.Hour

// piSessionMaxAge bounds transcript retention. Generous because a paused or
// failed-resumable run keeps its session id and resumes against it, and because
// a transcript is the run's own record rather than a credential left on disk.
const piSessionMaxAge = 30 * 24 * time.Hour

// piSweepStaleSeeds removes pi state an interrupted node left behind.
//
// Every seed is deleted by its own deferred cleanup, but a SIGKILL or an OOM
// skips that and strands a live access AND refresh token on disk with nothing
// to reap it.
//
// The age guard is the whole correctness of this function. The root is SHARED —
// by every node of the run under a sandbox, and off it by every run in the
// store — while iterion explicitly permits parallel branches and the studio
// runs several pipelines at once. So a `pi-agent-*` dir found here is just as
// likely to belong to a peer that is still running, and reaping it would pull
// the credential out from under a live pi process (whose own refresh then
// writes to a deleted path), surfacing as an intermittent "No API key found for
// openai-codex" for an operator who does have one.
//
// It also reaps stale SESSION transcripts under the same root. Those used to
// live inside the per-run worktree and vanish with it; on a shared root they
// outlive the run, are written by every pi node (not just codex ones), carry
// the node's whole conversation plus tool output, and no other cleanup reaches
// them — `iterion runs prune` walks `<store>/runs` only. Without this they grow
// without bound. Same age guard, for the same reason: pi writes into a live
// session as the node runs, and `--session-id` resume reads it back.
func piSweepStaleSeeds(root string) {
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), piSeedDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < piSeedMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
	piSweepStaleFiles(root, piSeedMaxAge, ".js", ".sysprompt.md")
	// Transcripts get a FAR longer bound than stranded credentials. They used to
	// live in the per-run worktree, which is deliberately preserved for a paused
	// or failed-resumable run — so reaping them on the credential's 12h bound
	// would make `--session-id` resume fail for a run paused over a weekend, on
	// a root now shared with every other run.
	piSweepStaleFiles(filepath.Join(root, "sessions"), piSessionMaxAge)
}

// piSweepStaleFiles age-reaps regular files directly in dir. With no suffixes
// every file is a candidate; with suffixes only those match, so a root shared
// with anything else is left alone.
func piSweepStaleFiles(dir string, maxAge time.Duration, suffixes ...string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if len(suffixes) > 0 && !hasAnySuffix(e.Name(), suffixes) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < maxAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func hasAnySuffix(name string, suffixes []string) bool {
	for _, sfx := range suffixes {
		if strings.HasSuffix(name, sfx) {
			return true
		}
	}
	return false
}

// piCodexExpiry works out the epoch-milliseconds deadline pi stores.
//
// The access token's own `exp` claim is the primary source, because the Codex
// CLI does not record a lifetime: real `~/.codex/auth.json` blobs carry
// `access_token`/`refresh_token`/`account_id`/`id_token` and `last_refresh`,
// and nothing in this tree ever writes `expires_in`. Deriving the deadline
// from `last_refresh + expires_in` therefore always yielded 0, and 0 is not
// the harmless default it looks like: pi treats it as expired and refreshes
// on EVERY node, spending an auth round-trip it did not need and rotating the
// operator's refresh token — the risk the file header describes as an
// exception was in fact the unconditional path.
//
// Falling back to 0 stays correct for a blob whose token cannot be read: pi
// then refreshes, which is better than sending a deadline we invented.
func piCodexExpiry(view secrets.CodexCredentialsView) int64 {
	if exp := jwtExpiryMillis(view.Tokens.AccessToken); exp > 0 {
		return exp
	}
	if view.LastRefresh == "" || view.Tokens.ExpiresIn <= 0 {
		return 0
	}
	last, err := time.Parse(time.RFC3339, view.LastRefresh)
	if err != nil {
		return 0
	}
	return last.Add(time.Duration(view.Tokens.ExpiresIn) * time.Second).UnixMilli()
}

// jwtExpiryMillis reads the `exp` claim of a JWT access token, in epoch ms.
//
// Epoch MILLISECONDS is pi's unit, not a guess: its own codex login writes
// `expires: Date.now() + expires_in * 1000`
// (packages/ai/src/auth/oauth/openai-codex.ts). Seconds here would put the
// deadline ~50 000 years out, and an already-dead access token would be sent
// verbatim with no refresh and no diagnostic.
//
// Only the payload is decoded and only one numeric claim is read — the
// signature is irrelevant here, since the token is not being trusted for
// authorization, merely asked when it stops working. pi decodes the same
// token for its account id. Returns 0 for anything unreadable.
func jwtExpiryMillis(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp * 1000
}
