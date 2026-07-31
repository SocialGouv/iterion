package delegate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

	// The opt-out governs what ITERION would spend. With no credential to
	// inject there is nothing to refuse, and a node running off the operator's
	// own `pi` /login — which this code path never reads — must still run.
	t.Run("the opt-out does not refuse when there is nothing to inject", func(t *testing.T) {
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		env, cleanup, err := piCodexSeed(context.Background(),
			Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}, nil)
		cleanup()
		if err != nil {
			t.Fatalf("err = %v — iterion holds no subscription credential here, so there is "+
				"nothing for the switch to refuse, and pi's own login must still work", err)
		}
		if len(env) != 0 {
			t.Errorf("env = %v, want none", env)
		}
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

// `--skill` must offer ONLY what ITERION wrote. The mirror directory is a
// checkout of the TARGET repo under `worktree: auto`, and CLI --skill paths
// bypass the project-trust gate `--no-approve` exists to close — so a repo
// shipping its own .claude/skills/ would otherwise get attacker-authored prompt
// text loaded as a trusted skill into every pi node.
//
// The list comes from the engine (Task.MirroredSkills). It cannot be recovered
// from the workspace: an earlier attempt read provenance markers the mirror
// leaves there, which a repo can forge because they sit inside the very
// checkout they were meant to vouch against.
func TestPiSkillArgs(t *testing.T) {
	// workspace lays out a mirror holding both iterion's skills and the repo's.
	workspace := func(t *testing.T, names ...string) string {
		t.Helper()
		work := t.TempDir()
		for _, n := range names {
			dir := filepath.Join(work, ".claude", "skills", n)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return work
	}
	skills := func(work string, names ...string) []string {
		out := make([]string, 0, len(names))
		for _, n := range names {
			out = append(out, filepath.Join(work, ".claude", "skills", n))
		}
		return out
	}
	joined := func(args []string) string { return strings.Join(args, " ") }

	t.Run("what the engine wrote is offered", func(t *testing.T) {
		work := workspace(t, "doc-enrichment")
		got := joined(piSkillArgs(Task{WorkDir: work, MirroredSkills: skills(work, "doc-enrichment")}, nil))
		if !strings.Contains(got, "doc-enrichment") {
			t.Errorf("args %q missing the bundle's skill — pi cannot see it otherwise", got)
		}
	})

	// The heart of it: the repo ships a skill the engine never wrote.
	t.Run("what the target repo ships is NOT offered", func(t *testing.T) {
		work := workspace(t, "doc-enrichment", "attacker-supplied")
		got := joined(piSkillArgs(Task{WorkDir: work, MirroredSkills: skills(work, "doc-enrichment")}, nil))
		if strings.Contains(got, "attacker-supplied") {
			t.Errorf("args %q include a repo-shipped skill — that routes around --no-approve", got)
		}
		if !strings.Contains(got, "doc-enrichment") {
			t.Error("iterion's own skill was dropped along with it")
		}
	})

	// A forged marker directory must change nothing: provenance is not read
	// back from the workspace at all any more.
	t.Run("forged provenance in the workspace is ignored", func(t *testing.T) {
		work := workspace(t, "attacker-supplied")
		markers := filepath.Join(work, ".claude", "skills", ".iterion-managed")
		if err := os.MkdirAll(markers, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(markers, "attacker-supplied.SKILL.md.sha256"),
			[]byte("forged"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := piSkillArgs(Task{WorkDir: work}, nil); len(got) != 0 {
			t.Errorf("args = %v — a repo forged its way past the gate", got)
		}
	})

	// Library skills reach pi the same way everything else does — reported by
	// the engine, which excludes the ones the workspace shadowed.
	t.Run("library skills count when the engine reports them", func(t *testing.T) {
		work := workspace(t, "changelog-writer")
		got := joined(piSkillArgs(Task{
			WorkDir:        work,
			MirroredSkills: skills(work, "changelog-writer"),
			SkillHints:     []SkillHint{{Name: "changelog-writer"}},
		}, nil))
		if !strings.Contains(got, "changelog-writer") {
			t.Errorf("args %q missing the library skill", got)
		}
	})

	// A hint is NOT provenance. One is recorded for every skill the workflow
	// references, including one the target repo pre-empted — it describes what
	// the agent will see, not who wrote it. Deriving `<dir>/<name>` from it
	// would hand the repo's own file over as a trusted skill.
	t.Run("a hint alone does not get the repo's file passed", func(t *testing.T) {
		work := workspace(t, "changelog-writer")
		got := piSkillArgs(Task{
			WorkDir:    work,
			SkillHints: []SkillHint{{Name: "changelog-writer"}},
		}, nil)
		if len(got) != 0 {
			t.Errorf("args = %v — a hint routed around the gate", got)
		}
	})

	t.Run("one flag per skill", func(t *testing.T) {
		work := workspace(t, "a", "b")
		args := piSkillArgs(Task{
			WorkDir:        work,
			MirroredSkills: skills(work, "a", "b", "a"),
			SkillHints:     []SkillHint{{Name: "b"}},
		}, nil)
		if n := strings.Count(joined(args), "--skill"); n != 2 {
			t.Errorf("%d flags for 2 skills (%q) — the payload is duplicated", n, joined(args))
		}
	})

	// The documented opt-in accepts the repo's own.
	t.Run("ITERION_PI_TRUST_PROJECT offers the whole directory", func(t *testing.T) {
		work := workspace(t, "attacker-supplied")
		t.Setenv("ITERION_PI_TRUST_PROJECT", "1")
		got := joined(piSkillArgs(Task{WorkDir: work}, nil))
		if got != "--skill "+filepath.Join(work, ".claude", "skills") {
			t.Errorf("args = %q, want the whole mirror directory", got)
		}
	})

	// The mirrors create .claude/skills lazily, so under the opt-in a bot with no
	// skills against a repo that ships none would otherwise get a --skill naming
	// a path that does not exist.
	t.Run("ITERION_PI_TRUST_PROJECT skips a directory that is not there", func(t *testing.T) {
		t.Setenv("ITERION_PI_TRUST_PROJECT", "1")
		if got := piSkillArgs(Task{WorkDir: t.TempDir()}, nil); len(got) != 0 {
			t.Errorf("args = %v, want none — that path does not exist", got)
		}
	})

	t.Run("nothing reported means nothing offered", func(t *testing.T) {
		work := workspace(t, "attacker-supplied")
		if got := piSkillArgs(Task{WorkDir: work}, nil); len(got) != 0 {
			t.Errorf("args = %v, want none", got)
		}
	})

	t.Run("no workdir", func(t *testing.T) {
		if got := piSkillArgs(Task{}, nil); len(got) != 0 {
			t.Errorf("args = %v, want none", got)
		}
	})

	// The failure worth a line in the run log: the engine says it wrote skills,
	// none of them are there, and pi runs with none while the bot's own prompt
	// orders the agent to load them. Silence here is the bug.
	t.Run("skills named but none resolved is reported", func(t *testing.T) {
		var buf bytes.Buffer
		logger := iterlog.New(iterlog.LevelWarn, &buf)
		work := workspace(t)
		args := piSkillArgs(Task{
			WorkDir:        work,
			MirroredSkills: skills(work, "vanished"),
		}, logger)
		if len(args) != 0 {
			t.Fatalf("args = %v, want none — the path does not exist", args)
		}
		if !strings.Contains(buf.String(), "no skills offered to pi") {
			t.Errorf("log = %q — the operator gets no explanation", buf.String())
		}
	})

	// ...and the ordinary path stays quiet.
	t.Run("resolved skills log nothing", func(t *testing.T) {
		var buf bytes.Buffer
		logger := iterlog.New(iterlog.LevelWarn, &buf)
		work := workspace(t, "doc-enrichment")
		piSkillArgs(Task{WorkDir: work, MirroredSkills: skills(work, "doc-enrichment")}, logger)
		if buf.Len() != 0 {
			t.Errorf("log = %q, want silence", buf.String())
		}
	})
}

// The argv builder is the only place that can see a node ended up with zero
// skills, so it is the only place that can say so — which means it needs the
// logger both transports have.
func TestPiExtraArgsForCarriesLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	work := t.TempDir()
	task := Task{
		NodeID:         "n",
		WorkDir:        work,
		MirroredSkills: []string{filepath.Join(work, ".claude", "skills", "vanished")},
	}

	backend := NewPiBackend(logger, "pi")
	if backend.print.Protocol.ExtraArgsFor == nil {
		t.Fatal("print transport has no per-task argv builder")
	}
	backend.print.Protocol.ExtraArgsFor(task)
	if !strings.Contains(buf.String(), "no skills offered to pi") {
		t.Errorf("print transport log = %q — the warning is unreachable", buf.String())
	}

	buf.Reset()
	piRPCArgs(task, "", logger)
	if !strings.Contains(buf.String(), "no skills offered to pi") {
		t.Errorf("rpc transport log = %q — the warning is unreachable", buf.String())
	}
}

// ITERION_PI_BIN is the documented escape hatch for a host that cannot run the
// npm CLI. It was documented and never implemented, so an operator who set it
// silently got the PATH binary instead.
func TestPiBinaryOverride(t *testing.T) {
	t.Run("env names the binary", func(t *testing.T) {
		t.Setenv("ITERION_PI_BIN", "/opt/pi-native")
		b := NewPiBackend(nil, "")
		if b.print.Command != "/opt/pi-native" {
			t.Errorf("print transport command = %q, want the override", b.print.Command)
		}
		rpc, ok := b.rpc.(*PiRPCBackend)
		if !ok || rpc.Command != "/opt/pi-native" {
			t.Errorf("rpc transport command = %+v, want the override on BOTH transports", b.rpc)
		}
	})

	// An explicit per-node `command:` is the more specific statement.
	t.Run("an explicit command wins", func(t *testing.T) {
		t.Setenv("ITERION_PI_BIN", "/opt/pi-native")
		if got := NewPiBackend(nil, "/usr/bin/pi").print.Command; got != "/usr/bin/pi" {
			t.Errorf("command = %q, want the explicit one", got)
		}
	})

	t.Run("unset leaves the protocol default", func(t *testing.T) {
		t.Setenv("ITERION_PI_BIN", "")
		if got := NewPiBackend(nil, "").print.Command; got != "" {
			t.Errorf("command = %q, want empty so the PATH lookup applies", got)
		}
	})
}

// A credential written inside the git worktree must be unstageable: a v2
// campaign agent runs `git add -A` before each in-stride commit, and
// finalizeWorktree fast-forwards the result onto the operator's branch.
//
// iterion writes its OWN guard at the seed root rather than trusting
// <workDir>/.iterion/.gitignore — that file is best-effort, is never
// overwritten when the repo already tracks one, and says nothing about a root
// that --store-dir put elsewhere under the worktree.
func TestPiCodexSeedMakesTheTokenUnstageable(t *testing.T) {
	guarded := func(t *testing.T, root string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
		if err != nil {
			t.Fatalf("no ignore guard at the seed root (%v) — `git add -A` would stage the token", err)
		}
		if !strings.Contains(string(data), "*") {
			t.Errorf("guard = %q, want the catch-all", data)
		}
	}

	t.Run("sandboxed: inside the workspace", func(t *testing.T) {
		work := t.TempDir()
		root, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, Sandbox: stubSandboxRun{}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		guarded(t, root)
	})

	// The dogfood invocation this repo prescribes — --store-dir "$PWD/.iterion" —
	// puts the store INSIDE the repo, so the non-sandboxed path carries the same
	// exposure.
	t.Run("a store dir inside the worktree", func(t *testing.T) {
		work := t.TempDir()
		store := filepath.Join(work, ".iterion")
		if err := os.MkdirAll(store, 0o755); err != nil {
			t.Fatal(err)
		}
		root, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: store}, nil)
		if err != nil {
			t.Fatal(err)
		}
		guarded(t, root)
	})

	// A root the repo already ignores wholesale needs nothing added.
	t.Run("an existing catch-all is left alone", func(t *testing.T) {
		work := t.TempDir()
		store := filepath.Join(work, ".iterion")
		root := filepath.Join(store, "pi")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("# mine\n*\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: store}, nil); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
		// Byte-identical: appending a second `*` would also keep "# mine", so
		// asserting survival alone does not pin the short-circuit.
		if string(data) != "# mine\n*\n" {
			t.Errorf("guard = %q, want it untouched", data)
		}
	})

	// --store-dir is operator-supplied and unconstrained, so the seed root can
	// land on a directory iterion did not create and does not own.
	t.Run("an unrelated ignore file is appended to, not replaced", func(t *testing.T) {
		work := t.TempDir()
		store := filepath.Join(work, ".iterion")
		root := filepath.Join(store, "pi")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("build/\n*.tmp"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: store}, nil); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
		if !strings.Contains(string(data), "build/") || !strings.Contains(string(data), "*.tmp") {
			t.Errorf("guard = %q — destroyed rules iterion did not author", data)
		}
		// The unterminated last line must not be swallowed into `*.tmp*`.
		if !slices.Contains(strings.Split(string(data), "\n"), "*") {
			t.Errorf("guard = %q — the catch-all never became its own line", data)
		}
	})

	// The workspace is a checkout of the target repository, which can commit
	// `.iterion` as a symlink — .gitignore does not stop a tracked symlink from
	// being checked out, and MkdirAll would follow it, writing the OAuth blob at
	// a repo-chosen host path.
	t.Run("a symlinked .iterion is refused, not followed", func(t *testing.T) {
		work, elsewhere := t.TempDir(), t.TempDir()
		if err := os.Symlink(elsewhere, filepath.Join(work, ".iterion")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, Sandbox: stubSandboxRun{}}, nil)
		if err == nil {
			t.Fatal("seeded through a repo-controlled symlink")
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Errorf("err = %v, want it to name the cause", err)
		}
		if _, err := os.Stat(filepath.Join(elsewhere, "pi")); err == nil {
			t.Error("created a directory at the symlink's target")
		}
	})

	// The same in-checkout write is reachable WITHOUT a sandbox: `--store-dir
	// "$PWD/.iterion"` is the invocation this repo's own dogfood instructions
	// prescribe, and the studio's workspace store has the same shape. Gating the
	// refusal on `sandboxed` left that path open.
	t.Run("a symlinked store dir inside the checkout is refused too", func(t *testing.T) {
		work, elsewhere := t.TempDir(), t.TempDir()
		if err := os.Symlink(elsewhere, filepath.Join(work, ".iterion")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		store := filepath.Join(work, ".iterion")
		_, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: store}, nil)
		if err == nil {
			t.Fatal("seeded through a repo-controlled symlink off the sandbox")
		}
		if _, err := os.Stat(filepath.Join(elsewhere, "pi")); err == nil {
			t.Error("created a directory at the symlink's target")
		}
	})

	// ...while a store genuinely outside the checkout is nobody's business but
	// the operator's, symlink or not.
	t.Run("a symlinked store outside the checkout is left alone", func(t *testing.T) {
		work, real, link := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "store")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: link}, nil); err != nil {
			t.Errorf("refused an operator's own symlinked store: %v", err)
		}
	})

	// --store-dir is taken VERBATIM, and `iterion schedule` renders it into cron
	// lines as given — so the root can be RELATIVE. That broke every guard here,
	// and the symlink refusal broke OPEN: filepath.Rel cannot relate an absolute
	// WorkDir to a relative root, so the containment test answered "no" and the
	// refusal never ran.
	t.Run("a relative store dir is absolutised before any guard reads it", func(t *testing.T) {
		work, elsewhere := t.TempDir(), t.TempDir()
		if err := os.Symlink(elsewhere, filepath.Join(work, ".iterion")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Chdir(work)

		_, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: ".iterion"}, nil)
		if err == nil {
			t.Fatal("a relative --store-dir walked straight past the symlink refusal")
		}
		if _, err := os.Stat(filepath.Join(elsewhere, "pi")); err == nil {
			t.Error("created a directory at the symlink's target")
		}
	})

	// And the root pi is told about must be absolute: pi's cwd is task.WorkDir
	// while iterion's is its own, so a relative PI_CODING_AGENT_DIR points at a
	// path where nothing was written — "No API key found for openai-codex".
	t.Run("the seed root handed to pi is absolute", func(t *testing.T) {
		work := t.TempDir()
		t.Chdir(work)
		root, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: "store"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(root) {
			t.Errorf("root = %q, want an absolute path", root)
		}
	})

	// MkdirAll is a NO-OP on a `.iterion/pi` the checkout pre-populated, and the
	// path guards only walk TO the seed root, never inside it. A repo can
	// therefore ship `.iterion/pi/.gitignore` as a tracked symlink: following it
	// appends into a host file of the repo's choosing, and git refuses to read a
	// symlinked .gitignore at all — so the credential would stay stageable.
	t.Run("a symlinked ignore guard inside the seed root is refused", func(t *testing.T) {
		work := t.TempDir()
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(work, ".iterion", "pi")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(root, ".gitignore")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		_, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, Sandbox: stubSandboxRun{}}, nil)
		if err == nil {
			t.Fatal("wrote the ignore guard through a repo-controlled symlink")
		}
		got, _ := os.ReadFile(victim)
		if string(got) != "original\n" {
			t.Errorf("victim file = %q — appended through the symlink", got)
		}
	})

	// gitignore is LAST-MATCH-WINS, so a `*` anywhere is not proof. The seed root
	// lives in the target repository's worktree, which can commit a `.gitignore`
	// that opens a hole below its own catch-all.
	t.Run("a catch-all negated below it does not count as guarded", func(t *testing.T) {
		for _, body := range []string{"*\n!auth.json\n", "*\n!*\n", "*\n!pi-agent-*\n"} {
			work := t.TempDir()
			store := filepath.Join(work, ".iterion")
			root := filepath.Join(store, "pi")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: store}, nil); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if last := strings.TrimSpace(lines[len(lines)-1]); last != "*" {
				t.Errorf("%q -> %q: last effective rule is %q, so the credential stays stageable",
					body, data, last)
			}
		}
	})

	// ...and a genuine trailing catch-all is still left alone.
	t.Run("a trailing catch-all short-circuits", func(t *testing.T) {
		work := t.TempDir()
		store := filepath.Join(work, ".iterion")
		root := filepath.Join(store, "pi")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "# mine\n!keep\n*\n\n# trailing comment\n"
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: store}, nil); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(filepath.Join(root, ".gitignore")); string(data) != body {
			t.Errorf("guard = %q, want it untouched", data)
		}
	})

	// Outside the worktree nothing is stageable, so no guard is written.
	t.Run("a store outside the worktree needs no guard", func(t *testing.T) {
		work, outside := t.TempDir(), t.TempDir()
		root, err := piCodexSeedRoot(Task{NodeID: "n", WorkDir: work, StoreDir: outside}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, ".gitignore")); err == nil {
			t.Error("wrote a guard outside the worktree — nothing there can be staged")
		}
	})
}

// The sweep is the ONLY thing that removes a seed stranded by SIGKILL/OOM, i.e.
// the only thing between an interrupted node and a live access + refresh token
// left on disk. Both directions matter: reaping a LIVE peer's dir is the worse
// failure, and is why the age bound is generous rather than tight.
func TestPiSweepStaleSeeds(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, age time.Duration) string {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	stale := mk("pi-agent-stale", piSeedMaxAge+time.Hour)
	fresh := mk("pi-agent-fresh", time.Minute)
	// Not ours: the sweep must not treat the root as its own scratch space.
	foreign := mk("something-else", piSeedMaxAge+time.Hour)

	piSweepStaleSeeds(root)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a stranded seed survived (%v) — a live credential is left on disk with nothing to reap it", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("reaped a fresh peer's seed (%v) — its pi would refresh into a deleted path", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("deleted an unrelated directory (%v)", err)
	}
}

// An operator who pinned pi's own PI_CODING_AGENT_DIR gets left alone: it is
// the more natural variable to reach for, and the seed would otherwise
// overwrite it through task.ExtraEnv.
func TestPiCodexSeedRespectsPisOwnAgentDir(t *testing.T) {
	for _, v := range []string{"ITERION_PI_AGENT_DIR", "PI_CODING_AGENT_DIR"} {
		t.Run(v, func(t *testing.T) {
			writeCodexAuth(t, "chatgpt")
			t.Setenv(v, "/operator/pinned")
			env, cleanup, err := piCodexSeed(context.Background(),
				Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", StoreDir: t.TempDir()}, nil)
			cleanup()
			if err != nil || len(env) != 0 {
				t.Errorf("env=%v err=%v — the operator's own agent dir must win", env, err)
			}
		})
	}
}

// pi writes its extension bundle, composed system prompt, session transcripts
// and (sandboxed) seeded credential under <WorkDir>/.iterion/pi. A TRACKED
// symlink there survives a checkout, and both MkdirAll and pi follow it.
func TestPiGuardWriteRoot(t *testing.T) {
	t.Run("a symlinked .iterion/pi is refused", func(t *testing.T) {
		work, elsewhere := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(work, ".iterion"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(work, ".iterion", "pi")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := piGuardWriteRoot(work); err == nil {
			t.Fatal("ran with pi's write root redirected by the repo")
		}
	})

	// The operator's own `.iterion` may legitimately point at another volume:
	// without `worktree: auto` WorkDir is their repo root and `.iterion` is the
	// conventional store dir. Refusing that would fail every pi node on a
	// working setup.
	t.Run("a symlinked .iterion is allowed", func(t *testing.T) {
		work, volume := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(volume, "pi"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(volume, filepath.Join(work, ".iterion")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := piGuardWriteRoot(work); err != nil {
			t.Errorf("refused an operator's own store symlink: %v", err)
		}
	})

	t.Run("a real directory and an absent one both pass", func(t *testing.T) {
		work := t.TempDir()
		if err := piGuardWriteRoot(work); err != nil {
			t.Errorf("absent root refused: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(work, ".iterion", "pi"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := piGuardWriteRoot(work); err != nil {
			t.Errorf("real directory refused: %v", err)
		}
		if err := piGuardWriteRoot(""); err != nil {
			t.Errorf("no workdir refused: %v", err)
		}
	})
}

// The structural change: a sandboxed run no longer writes its state inside the
// target repository's checkout when host_state gave it a shared mount. That is
// what retires the guard family for the default path — there is no repo-owned
// directory left to defend.
func TestPiStateRootPrefersTheSharedMount(t *testing.T) {
	work, shared := t.TempDir(), t.TempDir()

	t.Run("sandboxed with a shared mount stays out of the checkout", func(t *testing.T) {
		root := piStateRoot(Task{WorkDir: work, SharedStateDir: shared, Sandbox: stubSandboxRun{}})
		if want := filepath.Join(shared, "pi"); root != want {
			t.Fatalf("root = %q, want %q", root, want)
		}
		if lexicallyWithin(work, root) {
			t.Error("root is inside the target repository's checkout")
		}
	})

	// No mount (host_state=none, or the kubernetes driver): the workspace bind
	// is the only thing the container can read, so there is no choice.
	t.Run("sandboxed without one falls back into the workspace", func(t *testing.T) {
		root := piStateRoot(Task{WorkDir: work, Sandbox: stubSandboxRun{}})
		if want := filepath.Join(work, ".iterion", "pi"); root != want {
			t.Fatalf("root = %q, want the in-workspace fallback %q", root, want)
		}
	})

	// Unchanged off the sandbox: the store still wins, so transcripts keep
	// sharing the store's lifetime and `iterion runs prune` still reaches them.
	t.Run("off the sandbox the store still wins", func(t *testing.T) {
		store := t.TempDir()
		if root := piStateRoot(Task{WorkDir: work, StoreDir: store}); root != filepath.Join(store, "pi") {
			t.Errorf("root = %q, want the store's pi dir", root)
		}
	})

	// A shared mount is only meaningful inside a sandbox; on the host the store
	// is still the right home.
	t.Run("the shared mount does not override the store off the sandbox", func(t *testing.T) {
		store := t.TempDir()
		root := piStateRoot(Task{WorkDir: work, StoreDir: store, SharedStateDir: shared})
		if root != filepath.Join(store, "pi") {
			t.Errorf("root = %q, want the store's pi dir", root)
		}
	})
}

// The seeded credential is what the whole guard family was protecting. With a
// shared mount it simply is not in the checkout any more, so nothing needs
// guarding — no ignore file is written into the repo, and none is needed.
func TestPiCodexSeedRootOutsideTheCheckoutNeedsNoGuard(t *testing.T) {
	work, shared := t.TempDir(), t.TempDir()
	root, err := piCodexSeedRoot(Task{
		NodeID: "n", WorkDir: work, SharedStateDir: shared, Sandbox: stubSandboxRun{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); err == nil {
		t.Error("wrote an ignore guard outside the checkout — nothing there can be staged")
	}
	if _, err := os.Stat(filepath.Join(work, ".iterion")); err == nil {
		t.Error("touched the target repository's tree at all")
	}
}
