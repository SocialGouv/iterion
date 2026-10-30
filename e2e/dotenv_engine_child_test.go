package e2e

// The `.env` auto-load through the REAL binary's main(), on the chain that
// carries it in production: a tool node's script re-enters the CLI through
// ITERION_ENGINE_BIN with cwd = the repository the run cloned, and that
// nested iterion used to apply the repository's `.env` to its own process —
// filling in the provider endpoint names nobody set on purpose, which is
// where the credential the engine resolved is spent (#2844).
//
// Observed from outside the process: what the nested run's own tool body
// reads from its environment. No LLM node, so nothing here costs anything.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

const (
	// endpointVarName is the class that matters: a provider endpoint, where a
	// credential is spent. Only its ABSENCE is asserted — a sibling change
	// refuses such a line at the loader itself when the process inherited a
	// credential for it, and this file must not contradict either outcome.
	endpointVarName = "ANTHROPIC_BASE_URL"
	// witnessVarName carries no refusal rule of its own, so it is what says
	// whether the loader RAN at all.
	witnessVarName = "DOTENV_ENGINE_CHILD_WITNESS"

	plantedEndpoint = "https://planted-by-the-repo.example/v1"
	plantedWitness  = "the-repository-wrote-this"
)

// childReading matches what a probe body prints, so the assertion reads the
// child's environment rather than the run's log around it.
var childReading = regexp.MustCompile(`endpoint=\[([^\]]*)\] witness=\[([^\]]*)\]`)

// probeBot is a tool-only workflow whose body prints what the PROCESS
// environment holds for the two planted names. `env_expansion: none` keeps
// the engine from answering `${…}` itself: the values printed are the ones
// the tool process's own shell reads, which are the ones its parent iterion
// held.
const probeBot = `tool inner:
  script: |
    printf 'endpoint=[%s] witness=[%s]' "${` + endpointVarName + `-}" "${` + witnessVarName + `-}"
  language: sh
  env_expansion: none

workflow main:
  entry: inner
  inner -> done
`

// dotEnvRepo writes the workspace a run would clone: a `.env` planting both
// names, and the probe workflow.
func dotEnvRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := endpointVarName + "=" + plantedEndpoint + "\n" + witnessVarName + "=" + plantedWitness + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.bot"), []byte(probeBot), 0o600); err != nil {
		t.Fatalf("write probe.bot: %v", err)
	}
	refuseAncestorDotEnv(t, dir)
	return dir
}

// writeReentryBot writes a workflow whose tool node does what
// bots/sec-audit-source does: read ITERION_ENGINE_BIN and launch a child run
// from the tree under audit. cdTo is empty when the node's own cwd is already
// that tree.
func writeReentryBot(t *testing.T, dir, cdTo, nestedStore string) {
	t.Helper()
	body := "tool dispatch:\n  script: |\n"
	if cdTo != "" {
		body += "    cd " + cdTo + " || exit 1\n"
	}
	body += "    \"$ITERION_ENGINE_BIN\" run probe.bot --store-dir " + nestedStore + " 2>&1 | sed -n 's/.*result: //p'\n"
	body += "  language: sh\n  env_expansion: none\n\nworkflow main:\n  entry: dispatch\n  dispatch -> done\n"
	if err := os.WriteFile(filepath.Join(dir, "dispatch.bot"), []byte(body), 0o600); err != nil {
		t.Fatalf("write dispatch.bot: %v", err)
	}
}

// refuseAncestorDotEnv fails loudly when a `.env` sits ABOVE the fixture:
// loadDotEnvFromCwd stops at the first one walking up, so an ancestor would
// make the control read nothing and the whole file prove nothing.
func refuseAncestorDotEnv(t *testing.T, dir string) {
	t.Helper()
	for cur := filepath.Dir(dir); ; {
		if _, err := os.Stat(filepath.Join(cur, ".env")); err == nil {
			t.Fatalf("%s exists: it shadows the fixture's own .env, so this test cannot judge anything", filepath.Join(cur, ".env"))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return
		}
		cur = parent
	}
}

// runIterion runs the real binary in workDir on an environment built from a
// small allowlist plus the entries the case needs, and returns its combined
// output.
//
// Not the host's environment minus a few names: a provider credential of the
// operator's, inherited, is a fact other guards read (a `.env` line naming an
// endpoint beside an inherited key is refused at the loader), so a test that
// let one through would judge a different question on a developer's machine
// than in CI.
func runIterion(t *testing.T, workDir string, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(iterionBinary(t), args...)
	cmd.Dir = workDir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		// Deterministic: a sandbox driver on the host would change which
		// filesystem the nested run reads, which is not what is under test.
		"ITERION_SANDBOX_DEFAULT=none",
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("iterion %v in %s: %v\n%s", args, workDir, err, out)
	}
	return string(out)
}

// childReadingOf extracts the endpoint and witness values the probe printed.
func childReadingOf(t *testing.T, out string) (endpoint, witness string) {
	t.Helper()
	m := childReading.FindAllStringSubmatch(out, -1)
	if len(m) != 1 {
		t.Fatalf("the probe body printed %d readings, want exactly one — the run did not reach it:\n%s", len(m), out)
	}
	return m[0][1], m[0][2]
}

// The control, and the convenience this change keeps: a human running the CLI
// inside a checkout still gets its `.env`. Without this case every skip below
// is unobservable — a loader that never ran would pass them all.
func TestAHumanCLIRunLoadsTheWorkspaceDotEnv(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	repo := dotEnvRepo(t)

	out := runIterion(t, repo, nil, "run", "probe.bot", "--store-dir", filepath.Join(t.TempDir(), "store"))
	if _, witness := childReadingOf(t, out); witness != plantedWitness {
		t.Fatalf("%s = %q, want the .env value %q", witnessVarName, witness, plantedWitness)
	}
}

// The production shape, and the whole point: the engine's own process sits
// where no `.env` reaches it (a runner pod's /home/iterion, here a directory
// of its own), its tool node re-enters the CLI inside the repository, and the
// nested iterion must not pick the repository's values up. The same `.env`,
// read from the same cwd, is live — the control above proves it.
func TestAnEngineReenteredIterionIgnoresTheWorkspaceDotEnv(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	repo := dotEnvRepo(t)
	engineDir := t.TempDir()
	refuseAncestorDotEnv(t, filepath.Join(engineDir, "x"))
	writeReentryBot(t, engineDir, repo, filepath.Join(t.TempDir(), "nested-store"))

	out := runIterion(t, engineDir, nil, "run", "dispatch.bot", "--store-dir", filepath.Join(t.TempDir(), "store"))
	endpoint, witness := childReadingOf(t, out)
	if endpoint != "" {
		t.Errorf("the re-entered iterion read %s = %q from the repository's .env; the engine's own process never had it", endpointVarName, endpoint)
	}
	if witness != "" {
		t.Errorf("the re-entered iterion read %s = %q: the loader ran where it must not", witnessVarName, witness)
	}
}

// And the operator's `.env` is not lost on the way down: when the human CLI
// invocation at the top DID load it, the value reaches the nested run the way
// every other variable does — through the environment its parent passed on.
// The skip removes a READ of the repository's file, not the inheritance.
func TestTheOperatorsDotEnvStillReachesAReenteredIterion(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	repo := dotEnvRepo(t)
	writeReentryBot(t, repo, "", filepath.Join(t.TempDir(), "nested-store"))

	out := runIterion(t, repo, nil, "run", "dispatch.bot", "--store-dir", filepath.Join(t.TempDir(), "store"))
	if _, witness := childReadingOf(t, out); witness != plantedWitness {
		t.Fatalf("%s = %q, want %q — the operator's own .env must still travel to a child", witnessVarName, witness, plantedWitness)
	}
}

// The named escape hatch, end to end: an operator who wants engine children
// to honour the workspace `.env` says so in the environment they launch from,
// and the value travels to the nested run with the rest of it.
func TestTheOperatorCanForceAReenteredIterionToLoadTheDotEnv(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	repo := dotEnvRepo(t)
	engineDir := t.TempDir()
	refuseAncestorDotEnv(t, filepath.Join(engineDir, "x"))
	writeReentryBot(t, engineDir, repo, filepath.Join(t.TempDir(), "nested-store"))

	out := runIterion(t, engineDir, []string{"ITERION_DOTENV_FORCE=1"},
		"run", "dispatch.bot", "--store-dir", filepath.Join(t.TempDir(), "store"))
	if _, witness := childReadingOf(t, out); witness != plantedWitness {
		t.Fatalf("%s = %q, want %q — the skip is a bound with no override", witnessVarName, witness, plantedWitness)
	}
}
