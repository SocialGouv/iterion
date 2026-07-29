package delegate

import (
	"context"
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
//   - pi refreshes an expired token and writes the result to the dir it was
//     given — here, the throwaway one. That keeps `~/.codex` untouched, but it
//     also means the refresh is not shared: if OpenAI rotates the refresh
//     token on use, the copy in `~/.codex/auth.json` can be invalidated and
//     the Codex CLI itself will need to re-login. There is no way to have both
//     without writing into the operator's credential file.

// piCodexProvider is pi's id for the ChatGPT-subscription provider.
const piCodexProvider = "openai-codex"

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

	view, err := piLoadCodexView(ctx)
	if err != nil {
		return nil, noop, fmt.Errorf(
			"pi backend: node %q targets %s but no ChatGPT credential could be read (%w). "+
				"Sign in with `codex login`, or run `pi` and use /login",
			task.NodeID, piCodexProvider, err)
	}
	if !view.IsChatGPTMode() {
		return nil, noop, fmt.Errorf(
			"pi backend: node %q targets %s but the Codex credential is not a ChatGPT "+
				"subscription (auth_mode=%q). That provider has no API-key path",
			task.NodeID, piCodexProvider, view.AuthMode)
	}

	dir, err := os.MkdirTemp(piCodexSeedRoot(task), "pi-agent-")
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
func piLoadCodexView(ctx context.Context) (secrets.CodexCredentialsView, error) {
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		if dir := creds.OAuthDir("codex"); dir != "" {
			return secrets.LoadCodexCredentialsFrom(dir)
		}
	}
	return secrets.LoadCodexCredentialsFromDisk()
}

// piCodexSeedRoot picks where the throwaway agent dir lives.
//
// The run's store dir is preferred so the file shares the store's lifetime and
// permissions rather than landing in a world-listable /tmp; os.MkdirTemp's own
// default is the fallback when a task carries no store.
func piCodexSeedRoot(task Task) string {
	if task.StoreDir == "" {
		return ""
	}
	root := filepath.Join(task.StoreDir, "pi")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return ""
	}
	return root
}

// piCodexExpiry converts Codex's expiry into the epoch-milliseconds pi stores.
//
// Codex records a refresh instant plus a lifetime; pi wants the deadline. When
// either is missing or unparseable the answer is 0, which pi reads as "expired"
// and refreshes from the refresh token — the safe direction, since guessing a
// deadline too far out would send a dead token upstream.
func piCodexExpiry(view secrets.CodexCredentialsView) int64 {
	if view.LastRefresh == "" || view.Tokens.ExpiresIn <= 0 {
		return 0
	}
	last, err := time.Parse(time.RFC3339, view.LastRefresh)
	if err != nil {
		return 0
	}
	return last.Add(time.Duration(view.Tokens.ExpiresIn) * time.Second).UnixMilli()
}
