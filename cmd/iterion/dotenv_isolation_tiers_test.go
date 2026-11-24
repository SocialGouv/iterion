package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/internal/isolationenv"
	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterruntime "github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The #1959 floor neutralizes a non-operator bot's `sandbox: none` by falling
// THROUGH to the operator's own tiers. This drives the REAL `.env` loader over
// a repository that answers for those tiers, and asks the production resolvers
// what the run gets — the bypass that made the floor's event a formality while
// the run executed on the host.
//
// The operator grade the tiers belong to is refused OUTRIGHT at the loader: the
// file a repository holds does not set the operator's posture, so the names
// never reach the live environment and never earn a planted mark — the attack
// this test drives is stopped one gate earlier than the planted-mark hiding.
func TestAPlantedSandboxTierCannotLowerTheFloorsFallThrough(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("HOME", t.TempDir())

	repo := t.TempDir()
	// Exactly what a sandboxed agent of a previous run can write: with no
	// worktree, its workspace IS the repository.
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte(
		isolationenv.SandboxDefault+"=none\n"+
			isolationenv.SandboxOverride+"=none\n"+
			isolationenv.SandboxFloor+"=off\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	for _, name := range []string{isolationenv.SandboxDefault, isolationenv.SandboxOverride, isolationenv.SandboxFloor} {
		if v, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s is already set (%q) — applyDotEnv would not plant it, and the probe would prove nothing", name, v)
		}
		// The loader PLANTS for the rest of the test binary; t.Setenv does not
		// cover a variable it never set.
		restoreEnv(t, name)
	}

	t.Chdir(repo)
	loadDotEnvFromCwd()

	// The refused names reach nothing: not the live environment, not the
	// planted marks the one reader consults.
	for _, name := range []string{isolationenv.SandboxDefault, isolationenv.SandboxOverride, isolationenv.SandboxFloor} {
		if got := os.Getenv(name); got != "" {
			t.Fatalf("a repository .env planted %s=%q — the operator grade is refused outright", name, got)
		}
		if envtrust.Planted(name) {
			t.Fatalf("%s is marked planted: the operator grade is refused, not marked", name)
		}
		if value, planted := isolationenv.Value(name); value != "" || planted {
			t.Errorf("the ONE reader must see an unset name for a refused %s: value=%q planted=%v", name, value, planted)
		}
	}

	// The tier the floor falls through to.
	global := iterruntime.ResolveGlobalSandboxDefault(nil)
	if global != "auto" {
		t.Errorf("the global sandbox default resolved %q from a repository `.env` — the floor neutralizes the bot's opt-out and then hands the run to the repository's answer", global)
	}

	// A team-authored bot declaring the opt-out: the #1959 case, end to end.
	wf := &ir.Workflow{Name: "team_bot", Sandbox: &ir.SandboxSpec{Mode: "none"}}
	floored, neutralized := iterruntime.ApplySandboxFloor(wf, store.BotSourceTierTeam, "")
	if !neutralized {
		t.Fatalf("the floor did not fire — a planted %s=off disarmed it", isolationenv.SandboxFloor)
	}
	if !iterruntime.WorkflowSandboxActive(floored, "", global) {
		t.Errorf("the floor fired and the run STILL resolves unsandboxed: a repository `.env` chose the tier the floor defers to")
	}

	// The cloud side reads the same two tiers through the config, and must
	// drop them for the same reason — with the ignored names named.
	cfg, err := iterconfig.Load(iterconfig.LoadOptions{})
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	if cfg.Sandbox.Default != "" || cfg.Sandbox.Override != "" {
		t.Errorf("the deployment config took the repository's answer: default=%q override=%q", cfg.Sandbox.Default, cfg.Sandbox.Override)
	}
	// The operator grade is refused at the loader, one gate before the
	// config: nothing is planted, so there is no ignored plant to travel
	// with the config — the refusal is the loader's own diagnostic.
	if len(cfg.Sandbox.PlantedIgnored) != 0 {
		t.Errorf("an operator-grade name must be refused at the loader, not recorded as an ignored plant: %v", cfg.Sandbox.PlantedIgnored)
	}
}
