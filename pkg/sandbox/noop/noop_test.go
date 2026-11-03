package noop

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestNoopName(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Name() != "noop" {
		t.Errorf("Name = %q, want noop", d.Name())
	}
}

func TestNoopCapabilitiesAllFalse(t *testing.T) {
	d, _ := New()
	caps := d.Capabilities()
	zero := sandbox.Capabilities{}
	if caps != zero {
		t.Errorf("Capabilities = %+v, want all-false", caps)
	}
}

func TestNoopPrepareInactive(t *testing.T) {
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone})
	if err != nil {
		t.Fatalf("Prepare(none): %v", err)
	}
	p, ok := prepared.(*Prepared)
	if !ok {
		t.Fatalf("PreparedSpec type = %T, want *Prepared", prepared)
	}
	if p.SkippedReason != "" {
		t.Errorf("SkippedReason for none = %q, want empty", p.SkippedReason)
	}
}

func TestNoopPrepareActiveProducesSkipReason(t *testing.T) {
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeAuto})
	if err != nil {
		t.Fatalf("Prepare(auto): %v", err)
	}
	p := prepared.(*Prepared)
	if p.SkippedReason == "" {
		t.Error("expected SkippedReason for active mode, got empty")
	}
	if !strings.Contains(p.SkippedReason, "auto") {
		t.Errorf("SkippedReason should mention the requested mode: %q", p.SkippedReason)
	}
}

func TestNoopPrepareRejectsInvalidSpec(t *testing.T) {
	d, _ := New()
	_, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.Mode("bogus")})
	if err == nil {
		t.Fatal("expected validation error for invalid mode")
	}
}

func TestNoopExecCapturesStdout(t *testing.T) {
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(context.Background())

	res, err := run.Exec(context.Background(), []string{"sh", "-c", "printf hello"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if string(res.Stdout) != "hello" {
		t.Errorf("Stdout = %q, want %q", string(res.Stdout), "hello")
	}
}

func TestNoopExecPropagatesNonZeroExit(t *testing.T) {
	d, _ := New()
	prepared, _ := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone})
	run, _ := d.Start(context.Background(), prepared, sandbox.RunInfo{})
	defer run.Cleanup(context.Background())

	res, err := run.Exec(context.Background(), []string{"sh", "-c", "exit 42"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec returned err for non-zero exit: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
}

func TestConstructorMatchesNew(t *testing.T) {
	a, _ := New()
	b, _ := Constructor()
	if a.Name() != b.Name() {
		t.Errorf("Constructor and New disagree: %q vs %q", a.Name(), b.Name())
	}
}

func TestNoopExecDeliversTheSpecSeededEnv(t *testing.T) {
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode: sandbox.ModeNone,
		Env:  map[string]string{"ITERION_TEST_SEED": "seeded", "ITERION_TEST_BOTH": "from-spec"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(context.Background())

	res, err := run.Exec(context.Background(), []string{"sh", "-c", "printf %s \"$ITERION_TEST_SEED|$ITERION_TEST_BOTH\""},
		sandbox.ExecOpts{Env: map[string]string{"ITERION_TEST_BOTH": "from-opts"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d", res.ExitCode)
	}
	if got, want := string(res.Stdout), "seeded|from-opts"; got != want {
		t.Errorf("stdout = %q, want %q — the spec's seeded env must reach a pinned noop command, and an ExecOpts entry must still win", got, want)
	}
}

// The host passthrough keeps what the operator's own environment carries:
// a spec-seeded key the ambient environment already has keeps its ambient
// value — the same rule seedTreeNoiseEnv applies to an operator export —
// while a key the ambient lacks still lands.
func TestNoopExecAmbientEnvWinsOverTheSpecSeed(t *testing.T) {
	t.Setenv("ITERION_TEST_AMBIENT", "from-operator")
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode: sandbox.ModeNone,
		Env: map[string]string{
			"ITERION_TEST_AMBIENT": "from-spec",
			"ITERION_TEST_GAP":     "seeded",
		},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(context.Background())

	res, err := run.Exec(context.Background(), []string{"sh", "-c", `printf %s "$ITERION_TEST_AMBIENT|$ITERION_TEST_GAP"`}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got, want := string(res.Stdout), "from-operator|seeded"; got != want {
		t.Errorf("stdout = %q, want %q — the operator's ambient value must win, the seed must fill the gap", got, want)
	}
}

// An explicitly EMPTY ambient export is not a claim — the same rule
// seedTreeNoiseEnv applies to an operator export — so the spec's seed
// lands over it: an empty ITERION_TREE_NOISE must never silence the host
// gate's canonical pathspec list.
func TestNoopExecEmptyAmbientExportIsNotAClaim(t *testing.T) {
	t.Setenv("ITERION_TEST_EMPTY", "")
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode: sandbox.ModeNone,
		Env:  map[string]string{"ITERION_TEST_EMPTY": "seeded"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(context.Background())

	res, err := run.Exec(context.Background(), []string{"sh", "-c", `printf %s "$ITERION_TEST_EMPTY"`}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got, want := string(res.Stdout), "seeded"; got != want {
		t.Errorf("stdout = %q, want %q — an empty ambient export must not swallow the seed", got, want)
	}
}

// noopWorkDirHelperEnv turns the test binary into the child of the witness
// below. A compiled child is the point: a POSIX shell recomputes PWD from its
// real cwd at startup, so a shell command reports the right directory with or
// without the gesture and cannot redden.
const noopWorkDirHelperEnv = "ITERION_NOOP_WORKDIR_HELPER"

const noopWorkDirHelperPrefix = "childPWD="

// TestNoopWorkDirHelperChild prints the PWD its parent handed it. It is not a
// test of its own: without the marker it does nothing.
func TestNoopWorkDirHelperChild(t *testing.T) {
	if os.Getenv(noopWorkDirHelperEnv) == "" {
		return
	}
	fmt.Fprintf(os.Stdout, "\n%s%s\n", noopWorkDirHelperPrefix, os.Getenv("PWD"))
}

// TestNoopExecHandsTheChildItsWorkingDirectoryAsPWD: the noop driver is a HOST
// spawn with no image, so the gesture every host spawn of an agent CLI crosses
// (proc.SetWorkDir) is the driver's own — a CLI that resolves its project from
// PWD must read the run's workspace, not the engine's checkout.
func TestNoopExecHandsTheChildItsWorkingDirectoryAsPWD(t *testing.T) {
	dir := t.TempDir()
	d, _ := New()
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer run.Cleanup(context.Background())

	res, err := run.Exec(context.Background(),
		[]string{os.Args[0], "-test.run=^TestNoopWorkDirHelperChild$"},
		sandbox.ExecOpts{WorkDir: dir, Env: map[string]string{noopWorkDirHelperEnv: "1"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("helper child exited %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	got := ""
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), noopWorkDirHelperPrefix); ok {
			got = rest
		}
	}
	if got != dir {
		t.Fatalf("the child read PWD=%q, want the run's working directory %q — a CLI that resolves its project from PWD would work on the engine's checkout", got, dir)
	}
}
