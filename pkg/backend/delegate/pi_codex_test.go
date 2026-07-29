package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCodexAuth lays down a Codex CLI auth.json and points CODEX_HOME at it.
func writeCodexAuth(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	blob := map[string]any{
		"auth_mode": mode,
		"tokens": map[string]any{
			"access_token":  "access-abc",
			"refresh_token": "refresh-xyz",
			"account_id":    "acct-1",
			"expires_in":    3600,
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
		if cred.Type != "oauth" || cred.Access != "access-abc" || cred.Refresh != "refresh-xyz" {
			t.Errorf("credential = %+v, want pi's oauth shape carrying both tokens", cred)
		}
		want := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC).UnixMilli()
		if cred.Expires != want {
			t.Errorf("Expires = %d, want %d (last_refresh + expires_in, in ms)", cred.Expires, want)
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

	// A node that ASKED for this provider and cannot get it must fail loudly:
	// there is no API-key fallback for openai-codex, so degrading would run
	// the node with no credential and a confusing upstream error.
	t.Run("explicit failures", func(t *testing.T) {
		task := Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}

		t.Run("no credential", func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			if _, _, err := piCodexSeed(context.Background(), task, nil); err == nil {
				t.Fatal("no error although no ChatGPT credential exists")
			}
		})

		t.Run("api-key auth mode", func(t *testing.T) {
			writeCodexAuth(t, "apikey")
			_, _, err := piCodexSeed(context.Background(), task, nil)
			if err == nil || !strings.Contains(err.Error(), "not a ChatGPT") {
				t.Fatalf("err = %v, want a refusal naming the auth mode", err)
			}
		})

		t.Run("operator forbids subscription OAuth", func(t *testing.T) {
			writeCodexAuth(t, "chatgpt")
			t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
			_, _, err := piCodexSeed(context.Background(), task, nil)
			if err == nil || !strings.Contains(err.Error(), "ITERION_FORBID_SUBSCRIPTION_OAUTH") {
				t.Fatalf("err = %v, want the opt-out to be honoured and named", err)
			}
		})
	})

	// Guessing a deadline is the dangerous direction: a token believed live
	// but actually dead goes upstream and fails the node. 0 makes pi refresh.
	t.Run("an unusable expiry becomes 0 so pi refreshes", func(t *testing.T) {
		for name, view := range map[string]string{
			"missing last_refresh": `{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","account_id":"x","expires_in":3600}}`,
			"unparseable":          `{"auth_mode":"chatgpt","last_refresh":"not-a-date","tokens":{"access_token":"a","refresh_token":"r","account_id":"x","expires_in":3600}}`,
			"no expires_in":        `{"auth_mode":"chatgpt","last_refresh":"2026-07-29T10:00:00Z","tokens":{"access_token":"a","refresh_token":"r","account_id":"x"}}`,
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
