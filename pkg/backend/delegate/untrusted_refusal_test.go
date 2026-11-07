package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// countingCodex writes a fake `codex` that counts EVERY invocation — the
// capability probe included — answers `--version`, and fails anything else.
func countingCodex(t *testing.T) (bin string, spawns func() int) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "spawns")
	bin = filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"echo x >> '" + count + "'\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.153.4'; exit 0; fi\n" +
		"exit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() int {
		raw, _ := os.ReadFile(count)
		return strings.Count(string(raw), "x")
	}
}

// TestCodex_RefusesAnUntrustedWorkspaceBeforeSpawningAnything.
//
// codex has no floor: it loads the checkout's AGENTS.md family, its
// `.codex/skills`, and the MCP servers of its `.codex/config.toml`, which run
// on the host. So a fork workspace is refused, typed, before the binary is so
// much as probed; the trusted control must reach the binary, or the row
// proves nothing.
func TestCodex_RefusesAnUntrustedWorkspaceBeforeSpawningAnything(t *testing.T) {
	for _, row := range []struct {
		name    string
		trust   store.RunTrust
		refused bool
	}{
		{"a trusted workspace reaches codex", store.RunTrustDefault, false},
		{"a fork workspace is refused", store.RunTrustFork, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			bin, spawns := countingCodex(t)
			b := &CodexBackend{Command: bin, Logger: iterlog.Nop()}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := b.Execute(ctx, Task{NodeID: "n", UserPrompt: "review this diff", WorkDir: t.TempDir(), WorkspaceTrust: row.trust})
			if got := errors.Is(err, ErrUntrustedWorkspaceRefused); got != row.refused {
				t.Fatalf("refused = %v, want %v (err: %v)", got, row.refused, err)
			}
			if !row.refused {
				if spawns() == 0 {
					t.Fatal("the trusted control never reached the codex binary — this row proves nothing")
				}
				return
			}
			if n := spawns(); n != 0 {
				t.Errorf("codex was spawned %d time(s) on a workspace it refuses", n)
			}
			assertRefusalNamesTheWayOut(t, err, BackendCodex)
		})
	}
}

// assertRefusalNamesTheWayOut: an operator reading the refusal must learn
// which backend refused and what to do about it.
func assertRefusalNamesTheWayOut(t *testing.T, err error, backend string) {
	t.Helper()
	for _, want := range []string{backend, "trusted workspace", "backend with a floor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// TestCLIAgents_AnUnansweredUntrustedPolicyRefuses.
//
// Every CLI agent reaches the checkout through CLIAgentBackend.Execute, and
// the policy's zero value refuses. grok and kimi leave it unanswered — nobody
// has measured what those CLIs load from a project directory — so they refuse
// a fork workspace, typed, and so does opencode, whose AGENTS.md was measured
// reaching the model; pi answers it with its own floor, which the guarantee
// bench watches, and must NOT be refused by this gate.
func TestCLIAgents_AnUnansweredUntrustedPolicyRefuses(t *testing.T) {
	fork := Task{NodeID: "n", UserPrompt: "review", WorkDir: t.TempDir(), WorkspaceTrust: store.RunTrustFork}
	trusted := fork
	trusted.WorkspaceTrust = store.RunTrustDefault
	missing := filepath.Join(t.TempDir(), "no-such-binary")

	for _, row := range []struct {
		name string
		b    Backend
	}{
		{"grok", NewGrokBackend(iterlog.Nop(), missing)},
		{"kimi", NewKimiBackend(iterlog.Nop(), missing)},
		// Measured, not assumed: its AGENTS.md reaches the model.
		{"opencode", NewOpenCodeBackend(iterlog.Nop(), missing)},
	} {
		t.Run(row.name+" refuses a fork workspace", func(t *testing.T) {
			_, err := row.b.Execute(context.Background(), fork)
			if !errors.Is(err, ErrUntrustedWorkspaceRefused) {
				t.Fatalf("err = %v, want ErrUntrustedWorkspaceRefused", err)
			}
			assertRefusalNamesTheWayOut(t, err, row.name)
		})
		t.Run(row.name+" is not refused on a trusted workspace", func(t *testing.T) {
			// It fails — the binary does not exist — but not on this gate.
			if _, err := row.b.Execute(context.Background(), trusted); errors.Is(err, ErrUntrustedWorkspaceRefused) {
				t.Fatalf("a trusted workspace was refused by the untrusted gate: %v", err)
			}
		})
	}

	t.Run("pi answers the policy with its own floor", func(t *testing.T) {
		b := &CLIAgentBackend{Protocol: piProtocol, Command: missing, Logger: iterlog.Nop()}
		if _, err := b.Execute(context.Background(), fork); errors.Is(err, ErrUntrustedWorkspaceRefused) {
			t.Fatalf("pi declares its own floor, and was refused by the gate anyway: %v", err)
		}
	})
}

// TestOpenCode_AForkIsRefusedBeforeTheProjectScreen: the screen's message
// offers ITERION_OPENCODE_TRUST_PROJECT=1, which cannot lift a refusal it does
// not govern. Setting it must change nothing, and the refusal must not offer
// it — even with executable project resources planted.
func TestOpenCode_AForkIsRefusedBeforeTheProjectScreen(t *testing.T) {
	t.Setenv("ITERION_OPENCODE_TRUST_PROJECT", "1")
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".opencode", "plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".opencode", "plugin", "x.ts"), []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewOpenCodeBackend(iterlog.Nop(), filepath.Join(t.TempDir(), "no-such-binary")).
		Execute(context.Background(), Task{NodeID: "n", UserPrompt: "review", WorkDir: ws, WorkspaceTrust: store.RunTrustFork})
	if !errors.Is(err, ErrUntrustedWorkspaceRefused) {
		t.Fatalf("err = %v, want the untrusted-workspace refusal", err)
	}
	if strings.Contains(err.Error(), "ITERION_OPENCODE_TRUST_PROJECT") {
		t.Errorf("a fork's refusal offers a grant that cannot lift it: %v", err)
	}
}

// TestRefusal_NamesOnlyTheBackendsThatHaveAFloor.
//
// The list of ways out is derived from the policy table, so a refused backend
// can never be offered as one — a hand-written list told opencode's operators
// to route the node to opencode, the day opencode was refused.
func TestRefusal_NamesOnlyTheBackendsThatHaveAFloor(t *testing.T) {
	floored := flooredBackends()
	if strings.Join(floored, ",") != "claude_code,claw,pi" {
		t.Fatalf("backends with a floor = %v, want [claude_code claw pi]", floored)
	}
	for name, policy := range untrustedWorkspacePolicies() {
		if policy != UntrustedWorkspaceRefused {
			continue
		}
		msg := refuseUntrustedWorkspace(name).Error()
		ways := msg[strings.LastIndex(msg, "("):]
		for other, p := range untrustedWorkspacePolicies() {
			if p == UntrustedWorkspaceRefused && strings.Contains(ways, other) {
				t.Errorf("%s's refusal offers %s, which is refused too: %s", name, other, msg)
			}
		}
	}
}

// TestEveryRegisteredBackendAnswersTheUntrustedPolicy: every backend the
// registry can hand a node answers the policy table — a new one cannot join
// the registry without saying what it does with a fork. (claw is registered
// from pkg/backend/model, so it is named explicitly.)
func TestEveryRegisteredBackendAnswersTheUntrustedPolicy(t *testing.T) {
	policies := untrustedWorkspacePolicies()
	names := []string{BackendClaw}
	for name := range DefaultRegistry(iterlog.Nop()).backends {
		names = append(names, name)
	}
	for _, name := range names {
		if _, ok := policies[name]; !ok {
			t.Errorf("backend %q is registered but absent from untrustedWorkspacePolicies", name)
		}
	}
}

// TestIOTask_CarriesTheHooksOnlyIterionCanVouchFor.
//
// The sandboxed claw path rebuilds the Task from an IOTask. On an untrusted
// workspace `InjectedHooks` is the ONLY authority that re-admits a hook group
// from the checkout's settings.json, so dropping it on the wire fails closed —
// and silently kills every plugin-contributed hook an operator enabled, on
// the path the fork lane is meant to be contained by.
func TestIOTask_CarriesTheHooksOnlyIterionCanVouchFor(t *testing.T) {
	injected := map[string][]any{"PreToolUse": {map[string]any{"matcher": "", "hooks": []any{}}}}
	wire, err := json.Marshal(ToIOTask(Task{
		NodeID:         "n1",
		WorkDir:        "/ws",
		WorkspaceTrust: store.RunTrustFork,
		InjectedHooks:  injected,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var back IOTask
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	got := FromIOTask(back)
	if got.WorkspaceTrust.Trusted() {
		t.Fatalf("the trust did not survive the wire: %q", got.WorkspaceTrust)
	}
	if len(got.InjectedHooks) != len(injected) {
		t.Fatalf("iterion's own hook groups did not survive the wire: %v\n%s", got.InjectedHooks, wire)
	}
	if _, ok := got.InjectedHooks["PreToolUse"]; !ok {
		t.Fatalf("the group name did not survive: %v", got.InjectedHooks)
	}
}
