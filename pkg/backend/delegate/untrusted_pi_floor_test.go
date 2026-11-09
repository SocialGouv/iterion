package delegate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WITNESSES (ADR-162 U3): pi's agent-configuration floors on a workspace its
// tenant did not write. Each drives the REAL argv/answer builder — the same
// functions both transports call — never a reimplementation:
//
//   - ITERION_PI_TRUST_PROJECT is an operator's standing statement about
//     checkouts they own; it cannot opt a fork into --skill on the checkout's
//     directory or into --approve (project-local TypeScript inside the
//     credentialed process).
//   - --no-context-files is forced: the checkout's AGENTS.md/CLAUDE.md are
//     injected as INSTRUCTIONS into the agent reviewing their author's diff.
//   - piAmbientFiles withholds the workspace's half of the ambient policy
//     (the operator's half names no file the checkout can write).
//   - the ChatGPT-forfait bridge neither seeds a token where the fork's agent
//     reads nor falls back to the host's own login.
//
// Mutants: drop `&& task.WorkspaceTrust.Trusted()` from either TRUST_PROJECT
// reader → the first two rows redden; from the context-files condition → the
// third; from piAmbientFiles → the fourth; from piCodexSeed (its trust gate
// or its StateInCheckout-only reading) → the fifth and seventh; from
// piLoadCodexView → the sixth.
func TestPi_UntrustedWorkspaceFloors(t *testing.T) {
	t.Run("TRUST_PROJECT cannot opt a fork into the checkout's skills", func(t *testing.T) {
		work := t.TempDir()
		dir := filepath.Join(work, ".claude", "skills")
		if err := os.MkdirAll(filepath.Join(dir, "repo-skill"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ITERION_PI_TRUST_PROJECT", "1")

		fork := piSkillArgs(Task{WorkDir: work, WorkspaceTrust: store.RunTrustFork}, nil)
		if slices.Contains(fork, dir) {
			t.Errorf("fork args = %v — the checkout's skills directory was handed over as trusted", fork)
		}
		trusted := piSkillArgs(Task{WorkDir: work}, nil)
		if !slices.Contains(trusted, dir) {
			t.Errorf("trusted control: args = %v, want --skill %s (the opt-in still works where it exists)", trusted, dir)
		}
	})

	t.Run("TRUST_PROJECT cannot opt a fork into --approve", func(t *testing.T) {
		t.Setenv("ITERION_PI_TRUST_PROJECT", "1")
		if args := piExtraArgsFor(Task{WorkspaceTrust: store.RunTrustFork}, nil); slices.Contains(args, "--approve") {
			t.Errorf("fork args = %v — project-local TypeScript would execute inside the credentialed process", args)
		}
		if args := piExtraArgsFor(Task{}, nil); !slices.Contains(args, "--approve") {
			t.Errorf("trusted control: args = %v, want --approve", args)
		}
	})

	t.Run("the checkout's context files are never loaded", func(t *testing.T) {
		// `all` is the one policy that lets pi load them natively — the floor
		// must hold exactly there.
		fork := piExtraArgsFor(Task{AmbientContext: ambient.All, WorkspaceTrust: store.RunTrustFork}, nil)
		if !slices.Contains(fork, "--no-context-files") {
			t.Errorf("fork args = %v, want --no-context-files under every policy", fork)
		}
		trusted := piExtraArgsFor(Task{AmbientContext: ambient.All}, nil)
		if slices.Contains(trusted, "--no-context-files") {
			t.Errorf("trusted control: args = %v — policy `all` keeps pi's native loading", trusted)
		}
	})

	t.Run("the ambient suffix withholds the checkout's files, keeps the operator's", func(t *testing.T) {
		agentDir, lab, repo, work := piTree(t)
		ws := piAmbientFiles(Task{WorkDir: work, AmbientContext: ambient.Workspace, WorkspaceTrust: store.RunTrustFork})
		if len(ws) != 0 {
			t.Errorf("fork workspace policy carries %v — the checkout's prose reaches the reviewing agent as instruction", paths(ws))
		}
		op := piAmbientFiles(Task{WorkDir: work, AmbientContext: ambient.Operator, WorkspaceTrust: store.RunTrustFork})
		want := []string{filepath.Join(agentDir, "AGENTS.md"), filepath.Join(lab, "CLAUDE.md")}
		if !slices.Equal(paths(op), want) {
			t.Errorf("fork operator files = %v, want %v — the floor withdraws the checkout's half, not the operator's", paths(op), want)
		}
		for _, p := range paths(op) {
			if strings.HasPrefix(p, repo) {
				t.Errorf("fork suffix carries the checkout's %s", p)
			}
		}
	})

	t.Run("the bridge never seeds a token into the fork's checkout", func(t *testing.T) {
		writeCodexAuth(t, "chatgpt")
		work := t.TempDir()
		var buf bytes.Buffer
		logger := iterlog.New(iterlog.LevelWarn, &buf)
		// WorkDir with no store/sandbox: the only seed root is StateInCheckout.
		task := Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", WorkDir: work, WorkspaceTrust: store.RunTrustFork}
		env, cleanup, err := piCodexSeed(context.Background(), task, logger)
		if err != nil {
			t.Fatalf("piCodexSeed: %v — stepping aside degrades with a note, it does not kill the node", err)
		}
		cleanup()
		if len(env) != 0 {
			t.Errorf("env = %v — a ChatGPT token was seeded where the fork's agent reads", env)
		}
		if _, serr := os.Stat(filepath.Join(work, ".iterion")); !os.IsNotExist(serr) {
			t.Errorf("the fork's checkout was written to (.iterion exists) — the seed stepped aside before creating anything")
		}
		if !strings.Contains(buf.String(), "never written where the fork's agent reads") {
			t.Errorf("the operator was not told why no credential came: %q", buf.String())
		}
	})

	// The U5 widening: the gate is no longer "the seed root happens to sit in
	// the checkout" but EVERY untrusted run. A shared state dir OUTSIDE the
	// checkout was StateShared — the old gate seeded there, handing the fork
	// a subscription to spend. The forfait is DELIVERED to the run (the ctx
	// copy), which piLoadCodexView reads for any trust: this row proves the
	// refusal is the trust gate, not a missing credential.
	t.Run("the bridge seeds nothing for a fork even outside the checkout", func(t *testing.T) {
		t.Setenv("ITERION_PI_AGENT_DIR", "")
		t.Setenv("PI_CODING_AGENT_DIR", "")
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
		forfait := t.TempDir()
		blob := `{"auth_mode":"chatgpt","tokens":{"access_token":"access-pi-run","refresh_token":"refresh-pi-run","account_id":"acct-pi-run","id_token":"id-pi-run"},"last_refresh":"2026-07-29T10:00:00Z"}`
		if err := os.WriteFile(filepath.Join(forfait, "auth.json"), []byte(blob), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
			OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindCodex): forfait},
		})

		work := t.TempDir()
		shared := t.TempDir() // outside work: StateShared, not StateInCheckout
		var buf bytes.Buffer
		logger := iterlog.New(iterlog.LevelWarn, &buf)
		task := Task{NodeID: "n", Model: "openai-codex/gpt-5.6-sol", WorkDir: work, SharedStateDir: shared, Sandbox: stubSandboxRun{}, WorkspaceTrust: store.RunTrustFork}
		if _, loc := task.StateDir(BackendPi); loc != StateShared {
			t.Fatalf("fixture: state location = %v, want StateShared — the row proves the gate outside the checkout", loc)
		}
		env, cleanup, err := piCodexSeed(ctx, task, logger)
		if err != nil {
			t.Fatalf("piCodexSeed: %v — stepping aside degrades with a note, it does not kill the node", err)
		}
		cleanup()
		if len(env) != 0 {
			t.Errorf("env = %v — a ChatGPT token was seeded for a fork to spend", env)
		}
		if _, serr := os.Stat(filepath.Join(shared, BackendPi)); !os.IsNotExist(serr) {
			t.Errorf("the shared state root was written to — the seed stepped aside before creating anything")
		}
		if !strings.Contains(buf.String(), "never written where the fork's agent reads") {
			t.Errorf("the operator was not told why no credential came: %q", buf.String())
		}

		// Trusted control, same task, same delivered forfait: the seed runs,
		// the bridge funds the node — the floor is the fork lane's, not the
		// bridge's.
		var ctl bytes.Buffer
		trusted := task
		trusted.WorkspaceTrust = store.RunTrustDefault
		env, cleanup, err = piCodexSeed(ctx, trusted, iterlog.New(iterlog.LevelWarn, &ctl))
		if err != nil {
			t.Fatalf("trusted control: piCodexSeed: %v", err)
		}
		defer cleanup()
		if env["PI_CODING_AGENT_DIR"] == "" {
			t.Fatalf("trusted control: env = %v, want the seeded agent dir", env)
		}
		cred := readSeededAuth(t, env)[piCodexProvider]
		if cred.Access != "access-pi-run" {
			t.Errorf("trusted control: seeded access = %q, want the delivered forfait's", cred.Access)
		}
	})

	t.Run("the host's own login is not the fallback for a fork", func(t *testing.T) {
		writeCodexAuth(t, "chatgpt") // a working subscription login EXISTS on this host
		_, err := piLoadCodexView(context.Background(), store.RunTrustFork)
		if err == nil || !strings.Contains(err.Error(), "not used for a workspace its tenant did not write") {
			t.Fatalf("err = %v — the fork would run on the operator's own ~/.codex login", err)
		}
	})
}

// TestPi_TheSkillHintOffersTheGrantOnlyWhereItWorks: the zero-skill warning
// names ITERION_PI_TRUST_PROJECT=1 as the way out — but on a fork that switch
// is ignored, so offering it sends the operator after a dial that does
// nothing.
func TestPi_TheSkillHintOffersTheGrantOnlyWhereItWorks(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "SKILL.md")
	for _, row := range []struct {
		name  string
		trust store.RunTrust
		offer bool
	}{
		{"trusted: the grant is the way out", store.RunTrustDefault, true},
		{"fork: the grant is ignored, so it is not offered", store.RunTrustFork, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			var buf bytes.Buffer
			_ = piSkillArgs(Task{NodeID: "n", WorkDir: t.TempDir(), WorkspaceTrust: row.trust, MirroredSkills: []string{missing}},
				iterlog.New(iterlog.LevelWarn, &buf))
			log := buf.String()
			if !strings.Contains(log, "no skills offered to pi") {
				t.Fatalf("the warning did not fire — this row proves nothing: %q", log)
			}
			if got := strings.Contains(log, "set ITERION_PI_TRUST_PROJECT=1"); got != row.offer {
				t.Errorf("offers the grant = %v, want %v: %s", got, row.offer, log)
			}
		})
	}
}

// TestPi_TheFloorRidesTheSharedArgvBuilder: pi's floors are asserted through
// the argv assembly EVERY pi spawn goes through (CLIAgentBackend.buildArgs —
// both transports bind it), not only through piExtraArgsFor called directly.
// Mutant: piProtocol's ExtraArgsFor rebound to drop the floors reddens both
// fork assertions.
func TestPi_TheFloorRidesTheSharedArgvBuilder(t *testing.T) {
	t.Setenv("ITERION_PI_TRUST_PROJECT", "1") // even with the operator's opt-in set
	b := &CLIAgentBackend{Protocol: piProtocol, Logger: testLogger()}
	fork, _ := b.buildArgs(piProtocol, Task{Model: "openai/gpt-5.5", WorkspaceTrust: store.RunTrustFork}, "review", "")
	if !slices.Contains(fork, "--no-context-files") {
		t.Errorf("fork argv = %v, want --no-context-files", fork)
	}
	if slices.Contains(fork, "--approve") {
		t.Errorf("fork argv = %v — the checkout's project trust was bought with the operator's switch", fork)
	}
	trusted, _ := b.buildArgs(piProtocol, Task{Model: "openai/gpt-5.5"}, "review", "")
	if !slices.Contains(trusted, "--approve") {
		t.Errorf("trusted control: argv = %v, want --approve (the opt-in still works where it exists)", trusted)
	}
}
