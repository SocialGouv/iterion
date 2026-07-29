package delegate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testAccessExpiry is the deadline the fixture's access token carries.
var testAccessExpiry = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// jwtWithExp builds an unsigned JWT carrying an `exp` claim. Only the payload
// matters: the deadline is read, never trusted for authorization.
func jwtWithExp(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// writeCodexAuth lays down a Codex CLI auth.json and points CODEX_HOME at it.
func writeCodexAuth(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	// Shaped like a REAL ~/.codex/auth.json: the Codex CLI writes no
	// `expires_in`, and nothing in this tree ever adds one. Inventing that
	// field is what let a bug ship green — the expiry silently collapsed to
	// 0 on every production credential, which pi reads as "expired".
	blob := map[string]any{
		"auth_mode": mode,
		"tokens": map[string]any{
			"access_token":  jwtWithExp(t, testAccessExpiry),
			"refresh_token": "refresh-xyz",
			"account_id":    "acct-1",
			"id_token":      "id-token",
		},
		"last_refresh": "2026-07-29T10:00:00Z",
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", dir)
	return dir
}

func readSeededAuth(t *testing.T, env map[string]string) map[string]piOAuthCredential {
	t.Helper()
	dir := env["PI_CODING_AGENT_DIR"]
	if dir == "" {
		t.Fatal("no PI_CODING_AGENT_DIR — pi would fall back to the operator's own (empty) agent dir")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("seeded auth.json unreadable: %v", err)
	}
	var got map[string]piOAuthCredential
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("seeded auth.json is not the shape pi parses: %v", err)
	}
	return got
}

func TestPiCodexSeed(t *testing.T) {
	t.Run("translates the Codex credential into pi's shape", func(t *testing.T) {
		writeCodexAuth(t, "chatgpt")
		task := Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}

		env, cleanup, err := piCodexSeed(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("piCodexSeed: %v", err)
		}
		defer cleanup()

		cred := readSeededAuth(t, env)[piCodexProvider]
		// The field names are pi's, not Codex's — that translation IS the
		// feature, since pi has no env-var path for this provider.
		if cred.Type != "oauth" || cred.Access == "" || cred.Refresh != "refresh-xyz" {
			t.Errorf("credential = %+v, want pi's oauth shape carrying both tokens", cred)
		}
		if cred.Expires != testAccessExpiry.UnixMilli() {
			t.Errorf("Expires = %d, want %d (the access token's own exp claim, in ms)",
				cred.Expires, testAccessExpiry.UnixMilli())
		}
	})

	t.Run("the seeded dir is removed on cleanup", func(t *testing.T) {
		writeCodexAuth(t, "chatgpt")
		task := Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}
		env, cleanup, err := piCodexSeed(context.Background(), task, nil)
		if err != nil {
			t.Fatal(err)
		}
		dir := env["PI_CODING_AGENT_DIR"]
		cleanup()
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("agent dir survived cleanup (%v) — a credential file must not outlive the node", err)
		}
	})

	// Pinning the agent dir hides the operator's own auth.json, so a node on
	// any other provider must be left alone — that credential breadth is half
	// the reason this backend exists.
	t.Run("no-op for a node on another provider", func(t *testing.T) {
		writeCodexAuth(t, "chatgpt")
		for _, model := range []string{"zai/glm-5.2", "anthropic/claude-opus-5", "openai/gpt-5.5"} {
			env, cleanup, err := piCodexSeed(context.Background(),
				Task{NodeID: "n", Model: model, StoreDir: t.TempDir()}, nil)
			cleanup()
			if err != nil {
				t.Fatalf("%s: %v", model, err)
			}
			if len(env) != 0 {
				t.Errorf("%s: seeded an agent dir (%v) — it would shadow the operator's own logins", model, env)
			}
		}
	})

	t.Run("an operator-pinned agent dir wins", func(t *testing.T) {
		writeCodexAuth(t, "chatgpt")
		t.Setenv("ITERION_PI_AGENT_DIR", "/operator/pinned")
		env, cleanup, err := piCodexSeed(context.Background(),
			Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}, nil)
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		if len(env) != 0 {
			t.Errorf("overrode the operator's pinned agent dir with %v — their own /login would be discarded", env)
		}
	})

	// No usable Codex credential must NOT fail the node. Before this bridge
	// existed such a node ran off pi's own /login store; erroring here would
	// take that working path away from an operator who has one.
	t.Run("steps aside when it has nothing to offer", func(t *testing.T) {
		task := Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}

		t.Run("no credential", func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			env, cleanup, err := piCodexSeed(context.Background(), task, nil)
			cleanup()
			if err != nil {
				t.Fatalf("err = %v — pi's own login is still a valid way to run this node", err)
			}
			if len(env) != 0 {
				t.Errorf("env = %v, want none so pi resolves its own credential", env)
			}
		})

		t.Run("api-key auth mode", func(t *testing.T) {
			writeCodexAuth(t, "apikey")
			env, cleanup, err := piCodexSeed(context.Background(), task, nil)
			cleanup()
			if err != nil || len(env) != 0 {
				t.Fatalf("env=%v err=%v — an api-key Codex login is not usable here, but pi may still have its own", env, err)
			}
		})
	})

	// The policy refusal is the one hard error: an operator who forbids
	// spending a personal plan must not have it happen behind their back.
	t.Run("the subscription opt-out is a hard refusal", func(t *testing.T) {
		writeCodexAuth(t, "chatgpt")
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		_, _, err := piCodexSeed(context.Background(),
			Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}, nil)
		if err == nil || !strings.Contains(err.Error(), "ITERION_FORBID_SUBSCRIPTION_OAUTH") {
			t.Fatalf("err = %v, want the opt-out honoured and named", err)
		}
	})

	// Only when the deadline is genuinely unknowable does it fall back to 0,
	// which pi reads as expired and refreshes. Reaching that on a NORMAL
	// credential is the bug this guards: it costs an auth round-trip per node
	// and rotates the operator's refresh token every time.
	t.Run("an unreadable expiry becomes 0 so pi refreshes", func(t *testing.T) {
		for name, view := range map[string]string{
			"opaque access token": `{"auth_mode":"chatgpt","tokens":{"access_token":"not-a-jwt","refresh_token":"r","account_id":"x"}}`,
			"malformed jwt":       `{"auth_mode":"chatgpt","tokens":{"access_token":"a.!!!.c","refresh_token":"r","account_id":"x"}}`,
			"no exp claim":        `{"auth_mode":"chatgpt","tokens":{"access_token":"eyJhbGciOiJub25lIn0.e30.sig","refresh_token":"r","account_id":"x"}}`,
		} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(view), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CODEX_HOME", dir)
				env, cleanup, err := piCodexSeed(context.Background(),
					Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				if got := readSeededAuth(t, env)[piCodexProvider].Expires; got != 0 {
					t.Errorf("Expires = %d, want 0 — a guessed deadline sends a dead token upstream", got)
				}
			})
		}
	})
}

// A bundle bot's skills are mirrored into <workDir>/.claude/skills/ without
// populating SkillHints (which carries only the DSL `skills:` library field).
// Gating --skill on the hints therefore hid every bundle skill from pi — the
// agent was told to load skills it had no way to see.
func TestPiSkillDir(t *testing.T) {
	mirror := func(t *testing.T, entries ...string) string {
		t.Helper()
		work := t.TempDir()
		dir := filepath.Join(work, ".claude", "skills")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if err := os.MkdirAll(filepath.Join(dir, e), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return work
	}

	t.Run("bundle skills are offered with no SkillHints", func(t *testing.T) {
		work := mirror(t, ".iterion-managed", "doc-enrichment", "docs-refresh")
		got := piSkillDir(Task{WorkDir: work})
		if want := filepath.Join(work, ".claude", "skills"); got != want {
			t.Errorf("piSkillDir = %q, want %q — pi cannot see a bundle's skills otherwise", got, want)
		}
	})

	t.Run("a mirror holding only bookkeeping offers nothing", func(t *testing.T) {
		work := mirror(t, ".iterion-managed")
		if got := piSkillDir(Task{WorkDir: work}); got != "" {
			t.Errorf("piSkillDir = %q, want empty — that directory advertises zero skills", got)
		}
	})

	t.Run("no mirror at all", func(t *testing.T) {
		if got := piSkillDir(Task{WorkDir: t.TempDir()}); got != "" {
			t.Errorf("piSkillDir = %q, want empty", got)
		}
	})

	// A sandboxed WorkDir names a path only the container can see, so the stat
	// fails; the hints prove the skills exist without it.
	t.Run("SkillHints short-circuit an unstattable WorkDir", func(t *testing.T) {
		task := Task{WorkDir: "/in-container/only", SkillHints: []SkillHint{{Name: "x"}}}
		if got := piSkillDir(task); got != filepath.Join("/in-container/only", ".claude", "skills") {
			t.Errorf("piSkillDir = %q, want the in-container mirror path", got)
		}
	})

	t.Run("no workdir", func(t *testing.T) {
		if got := piSkillDir(Task{}); got != "" {
			t.Errorf("piSkillDir = %q, want empty", got)
		}
	})
}

// A sandboxed run sees only the workspace: the store dir is not bind-mounted
// unless it happens to live under the global iterion home, and the repo's own
// dogfood invocation (--store-dir "$PWD/.iterion") plus every studio launch
// make it the workspace's — a SIBLING of the mounted worktree. Seeding there
// makes pi report "No API key found for openai-codex" to an operator who has
// one, on the DEFAULT path, since sandboxing is on by default.
func TestPiCodexSeedRootFollowsTheMount(t *testing.T) {
	work, store := t.TempDir(), t.TempDir()

	host := piCodexSeedRoot(Task{WorkDir: work, StoreDir: store})
	if host != filepath.Join(store, "pi") {
		t.Errorf("host root = %q, want it under the store", host)
	}

	boxed := piCodexSeedRoot(Task{WorkDir: work, StoreDir: store, Sandbox: stubSandboxRun{}})
	if boxed != filepath.Join(work, ".iterion", "pi") {
		t.Errorf("sandboxed root = %q, want it inside the workspace — the only mounted tree", boxed)
	}
	// It must also agree with where the session dir goes, since both have to
	// be reachable from inside the same container.
	if got := piSessionDir(Task{WorkDir: work, StoreDir: store, Sandbox: stubSandboxRun{}}); !strings.HasPrefix(got, work) {
		t.Errorf("session dir %q is outside the workspace; the two paths disagree", got)
	}
}

// A SIGKILL skips the deferred cleanup and strands a live access AND refresh
// token on disk. The dirs are per-node, so anything present when a new node
// starts is abandoned by definition.
func TestPiCodexSeedSweepsAbandonedDirs(t *testing.T) {
	writeCodexAuth(t, "chatgpt")
	store := t.TempDir()
	stale := filepath.Join(store, "pi", piSeedDirPrefix+"orphan")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "auth.json"), []byte(`{"x":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	env, cleanup, err := piCodexSeed(context.Background(),
		Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: store}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("abandoned credential dir survived (%v) — nothing else reaps it", err)
	}
	if _, err := os.Stat(env["PI_CODING_AGENT_DIR"]); err != nil {
		t.Errorf("the sweep took this node's own dir: %v", err)
	}
}
