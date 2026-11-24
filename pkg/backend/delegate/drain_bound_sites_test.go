package delegate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The two execs this package shells out to for a verdict — the probe that
// says whether the permission hook's binary is usable, and `codex --version`
// — read a child's output, so [exec.Cmd.Wait] waits for the copy of it, and
// a copier ends only when EVERY holder of the pipe has closed it. The
// context each site carries kills its own child and never what that child
// forked, so with no bound on the drain the wait ends when the leftover
// does: the start of a gated node, and a capability gate, park on a `sleep`
// (#2855, the class of #2469).
//
// One shape for both: the child prints its answer, leaves a process holding
// the output and exits 0. The wait is MEASURED against a deadline, so a
// regression fails in seconds instead of holding the package to its timeout.
//
// The two readings are deliberate and opposite, which is why the sites are
// witnessed together: the probe's verdict is an EXIT STATUS, so the cut
// keeps the success and is reported; the version gate's answer is the
// OUTPUT, so the cut is a failure naming the bound and the knob.

// leftoverHolderScript answers, leaves a process holding the output for
// holds, and exits 0. The script ignores SIGPIPE, so the holder stays alive
// once the bound closes the pipes under it and can report its own exit: no
// shell outlives the suite.
//
// holds is DERIVED from the site's own bound by leftoverHolds, never a
// constant: a holder that lets go at the moment the bound fires would make
// the verdict a race between a timer and a sleep.
func leftoverHolderScript(answer, done string, holds time.Duration) string {
	return fmt.Sprintf("( trap '' PIPE; sleep %.1f; : > %q ) &\nprintf '%%s\\n' %q\nexit 0",
		holds.Seconds(), done, answer)
}

// leftoverHolds is how long a holder outlives the bound under test. The
// margin is wall-clock slack on a loaded machine, where the bound's timer
// can fire late: the holder must still be there when it does, or the drain
// is never cut and the witness reports a defect the code does not have.
func leftoverHolds(bound time.Duration) time.Duration { return bound + 2*time.Second }

// awaitDrainBoundHolder joins the process the fixture left behind: it writes
// done as its last act, and a test that returned without it would leave a
// shell running past the suite.
func awaitDrainBoundHolder(t *testing.T, done string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(done); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the leftover holder never reported its exit (%s) — it may outlive the suite", done)
}

// Mutation seen red: drop the proc.SetWaitDelay call in
// preparePermissionHook — the probe's 5 s context kills the probe, the
// unbounded Wait keeps waiting for the output the holder has, and the gated
// node's start does not return within this test's deadline.
func TestThePermissionHookProbeReturnsWhileALeftoverHoldsItsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hook command fixture")
	}
	// The bound asserted below is the SITE's, so the environment this suite
	// runs in does not get to choose it.
	t.Setenv(proc.WaitDelayEnv, "")
	done := filepath.Join(t.TempDir(), "probe-holder-exited")
	iterionBin := filepath.Join(t.TempDir(), "iterion")
	body := "#!/bin/sh\n" + leftoverHolderScript("probe-ok", done, leftoverHolds(permissionHookProbeDrainBound))
	if err := os.WriteFile(iterionBin, []byte(body), 0o755); err != nil { // #nosec G306 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("ITERION_BIN", iterionBin)
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	t.Cleanup(func() { awaitDrainBoundHolder(t, done) })

	policy, err := permission.NewPolicy(permission.ModeDeny, []string{"Read(**)"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	backend := &CLIAgentBackend{Protocol: kimiProtocol, Logger: iterlog.New(iterlog.LevelWarn, &logs)}
	task := Task{NodeID: "n", Iteration: 1, StoreDir: t.TempDir(), Permission: policy}

	type outcome struct {
		cleanup func()
		err     error
	}
	got := make(chan outcome, 1)
	go func() {
		_, cleanup, err := backend.preparePermissionHook(context.Background(), task, kimiProtocol, BackendKimi)
		got <- outcome{cleanup: cleanup, err: err}
	}()
	var res outcome
	select {
	case res = <-got:
	case <-time.After(20 * time.Second):
		t.Fatalf("the gated node's start did not return in 20s: it waits for the output a process the probe left holds")
	}
	if res.cleanup != nil {
		t.Cleanup(res.cleanup)
	}
	// The probe's verdict is its exit status, and it exited 0: the gate arms.
	if res.err != nil {
		t.Fatalf("a probe that exited 0 with its output held refused the gate: %v", res.err)
	}
	out := logs.String()
	for _, want := range []string{"abandoned the drain", permissionHookProbeDrainBound.String(), proc.WaitDelayEnv} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q — the cut, the bound that ran, and the knob that widens it: %q", want, out)
		}
	}
}

// Mutation seen red: drop the proc.SetWaitDelay call in
// validateCodexWebSearchCapability — the version gate's 5 s context kills
// the shim, the unbounded Wait keeps waiting for the output the holder has,
// and the capability check does not return within this test's deadline.
func TestTheCodexVersionGateFailsNamingTheBoundWhenALeftoverHoldsItsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	// The bound asserted below is the SITE's, so the environment this suite
	// runs in does not get to choose it.
	t.Setenv(proc.WaitDelayEnv, "")
	done := filepath.Join(t.TempDir(), "codex-holder-exited")
	// A version line the parse WOULD accept: what fails the gate is the cut,
	// not the content, so a passing fixture is the only honest one here.
	command := writeCodexVersionFixtureScript(t, leftoverHolderScript("codex-cli 0.103.0", done, leftoverHolds(codexVersionDrainBound)))
	t.Cleanup(func() { awaitDrainBoundHolder(t, done) })

	type outcome struct {
		path string
		err  error
	}
	got := make(chan outcome, 1)
	go func() {
		path, err := validateCodexWebSearchCapability(context.Background(), command)
		got <- outcome{path: path, err: err}
	}()
	var res outcome
	select {
	case res = <-got:
	case <-time.After(20 * time.Second):
		t.Fatalf("the capability gate did not return in 20s: it waits for the output a process the CLI left holds")
	}
	// The answer this site gives is the version in those BYTES, and the copy
	// of them was abandoned mid-flight: a short read is not a verdict.
	if res.err == nil {
		t.Fatalf("a version read short was accepted as a verdict (path %q)", res.path)
	}
	for _, want := range []string{"read short", codexVersionDrainBound.String(), proc.WaitDelayEnv} {
		if !strings.Contains(res.err.Error(), want) {
			t.Errorf("the error does not name %q — the cut, the bound that ran, and the knob that widens it: %v", want, res.err)
		}
	}
}
