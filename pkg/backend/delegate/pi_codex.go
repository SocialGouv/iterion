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
	// possibly including a real `/login`. Overriding it would silently
	// discard that.
	if dir := strings.TrimSpace(os.Getenv("ITERION_PI_AGENT_DIR")); dir != "" {
		return nil, noop, nil
	}

	// This IS a subscription, so it answers to the same switch as the
	// Anthropic one: an operator who forbids spending a personal plan through
	// a third-party harness must not have it happen behind their back.
	if secrets.ForbidSubscriptionOAuth() {
		return nil, noop, fmt.Errorf(
			"pi backend: node %q targets the %s provider, which can only be driven by a ChatGPT "+
				"subscription token, but ITERION_FORBID_SUBSCRIPTION_OAUTH is set. Use an "+
				"OPENAI_API_KEY model (openai/…) instead, or unset the switch",
			task.NodeID, piCodexProvider)
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

	root := piCodexSeedRoot(task)
	piSweepStaleSeeds(root)
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
// A sandboxed run must place it inside the WORKSPACE, which is the only tree
// bind-mounted into the container. The store dir is not mounted unless it
// happens to sit under the global iterion home — and the repo's own dogfood
// invocation (`--store-dir "$PWD/.iterion"`) plus every studio launch make it
// the workspace's, whose `pi/` is a SIBLING of the mounted worktree under
// `worktree: auto`. pi then finds no auth.json, writes `{}`, and reports "No
// API key found for openai-codex" to an operator who has one. `piSessionDir`
// already branches this way for the same reason.
//
// Off the sandbox the run's store is preferred so the file shares the store's
// lifetime; os.MkdirTemp's own default is the last resort.
func piCodexSeedRoot(task Task) string {
	root := ""
	switch {
	case task.Sandbox != nil && task.WorkDir != "":
		root = filepath.Join(task.WorkDir, ".iterion", "pi")
	case task.StoreDir != "":
		root = filepath.Join(task.StoreDir, "pi")
	default:
		return ""
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return ""
	}
	return root
}

// piSweepStaleSeeds removes agent dirs an earlier node left behind.
//
// Every seed is deleted by its own deferred cleanup, but a SIGKILL or an OOM
// skips that and strands a live access AND refresh token on disk with nothing
// to reap it. The dirs are per-node and short-lived, so anything already here
// when a new node starts is by definition abandoned.
func piSweepStaleSeeds(root string) {
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), piSeedDirPrefix) {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
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
