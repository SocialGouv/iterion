//go:build linux

package scripthost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testHost spawns the REAL binary, built the way iterion ships. See TestMain
// for why a cgo-linked test binary cannot stand in for it here.
func testHost(t *testing.T) Host { return Host{Binary: realBinary(t)} }

// fakeChild writes a shell script that plays the child's part and returns a
// Host pointing at it — for the shapes a real child cannot produce: a broken
// deployment, a flood of stdout, a descendant that outlives its parent.
func fakeChild(t *testing.T, script string) Host {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-child")
	body := "#!/bin/sh\n[ \"$1\" = \"--probe\" ] && exit 0\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	// Wait until the kernel will exec it. A file written in one goroutine and
	// exec'd while OTHER parallel tests fork answers ETXTBSY: the fork
	// duplicates the still-open write descriptor into the child, and execve
	// refuses a file anyone holds for writing. Measured as a 2-in-10 flake
	// before this loop, which is worse than no test at all.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Command(path, "--probe").Run()
		if !errors.Is(err, syscall.ETXTBSY) {
			if err != nil {
				t.Fatalf("the fake child is not runnable: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake child stayed ETXTBSY for 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return Host{Binary: path}
}

// THE claim the whole package rests on, measured against the artefact that
// ships: the bounds cover the PROCESS, not the thread that applied them.
//
// no_new_privs and a Landlock domain are per-THREAD. Before they were driven
// across every thread, Confine returned `LandlockABI: 4` while a sibling
// thread read, wrote, connected and exec'd freely, and four goroutines out of
// four landed on unconfined threads. The child reports what it applied, so
// this reads the fact rather than inferring it.
func TestTheRealBinaryConfinesEveryThreadOfTheProcess(t *testing.T) {
	t.Parallel()

	resp, err := testHost(t).Run(context.Background(), "1+1", Limits{})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("refused: %+v", resp)
	}
	if resp.Confinement == nil {
		t.Fatal("the child reported no confinement: the posture is unobservable, which is how a " +
			"per-thread bound passed for a per-process one")
	}
	if !resp.Confinement.AllThreads {
		t.Error("the shipping binary confined only the thread that asked: the filesystem, socket and " +
			"exec bounds hold on one thread and not on the process")
	}
	if resp.Confinement.LandlockABI == 0 {
		t.Error("the shipping binary applied no Landlock ruleset")
	}
	if resp.Confinement.CPUSeconds == 0 || resp.Confinement.MemoryBytes == 0 {
		t.Errorf("a bound was not applied: %+v", resp.Confinement)
	}
}

func TestAConfinedChildRunsAScriptAndReturnsItsValue(t *testing.T) {
	t.Parallel()

	resp, err := testHost(t).Run(context.Background(),
		`(function(){var t=0; for(var i=1;i<=10;i++) t+=i; return {sum:t}})()`, Limits{})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("refused: %+v", resp)
	}
	if resp.Value != `{"sum":55}` {
		t.Errorf("value = %q, want {\"sum\":55}", resp.Value)
	}
}

// The clock is frozen in the CHILD, not merely wired in a unit test. Passing
// `epoch` to newRuntime and asserting it comes back tests the wiring; a mutant
// that made the child pass time.Now() survived that.
func TestTheChildFreezesItsClock(t *testing.T) {
	t.Parallel()

	resp, err := testHost(t).Run(context.Background(), "Date.now()", Limits{})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("refused: %+v", resp)
	}
	if want := strconv.FormatInt(epoch.UnixMilli(), 10); resp.Value != want {
		t.Errorf("the child's Date.now() = %s, want the fixed epoch %s — the child is reading the real clock",
			resp.Value, want)
	}
}

// The seed is the caller's, which is the whole content of "a run is
// replayable": the parent draws it and therefore holds it.
func TestTheSameSeedGivesTheSameSequence(t *testing.T) {
	t.Parallel()

	const script = `[Math.random(),Math.random()].join(",")`
	seed := strings.Repeat("ab", 32)

	one := childWithRequest(t, Request{Script: script, Seed: seed, AllowSingleThreadConfinement: true})
	two := childWithRequest(t, Request{Script: script, Seed: seed, AllowSingleThreadConfinement: true})
	if one.Value != two.Value {
		t.Errorf("the same seed gave %q then %q", one.Value, two.Value)
	}
	other := childWithRequest(t, Request{Script: script, Seed: strings.Repeat("cd", 32), AllowSingleThreadConfinement: true})
	if other.Value == one.Value {
		t.Error("two different seeds gave the same sequence: the request's seed is not reaching the PRNG")
	}
}

// THE bound this package exists for. A backreference regex routes goja onto
// regexp2, which is not linear-time and which goja's own interrupt cannot
// preempt: measured on the pinned engine, the same script ran past 60 s of
// wall clock with an interrupt posted at 1 s. The kernel's CPU limit is what
// stops it. ADR-098's F19 is the finding this answers.
func TestANativeRegexBombIsKilledByTheCPUBound(t *testing.T) {
	t.Parallel()

	// The host is built BEFORE the clock starts. realBinary compiles the
	// shipping artefact on first use (3.6 s warm, 19.5 s cold), and calling it
	// inside the timed window measured the build instead of the bound —
	// which is how this file first reported a 5.9 s "the interpreter did not
	// stop it" for a child that had stopped it at 2 s.
	h := testHost(t)

	start := time.Now()
	resp, err := h.Run(context.Background(),
		`/(a+)+\1b$/.test('a'.repeat(40))`,
		Limits{CPU: time.Second, Wall: time.Minute, Memory: 256 << 20})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != RefusalCPUTime {
		t.Fatalf("a native regex bomb gave %+v after %s, want a %s refusal.\n"+
			"If this says %s instead, the interpreter yielded and the premise of this package is wrong — "+
			"check whether goja started routing backreferences to a linear-time engine.",
			resp, elapsed.Round(time.Millisecond), RefusalCPUTime, RefusalWallClock)
	}
	if !strings.Contains(resp.Message, "CPU") {
		t.Errorf("the refusal does not name the CPU bound: %q", resp.Message)
	}
	// Killed on CPU, not waited out: the wall bound here is a minute. And the
	// bound is respected EXACTLY — equal soft and hard limits, so the kill
	// lands at the operator's number rather than a second past it.
	if elapsed > 5*time.Second {
		t.Errorf("the kill took %s — the CPU bound was not what stopped it", elapsed.Round(time.Second))
	}
}

// The ENFORCING half of the wall bound: the parent's own deadline, for the
// class the interpreter cannot interrupt. It had no witness at all — a mutant
// that deleted the parent's deadline, and one that disabled its branch, both
// left the suite green in 2 s.
func TestTheParentsDeadlineStopsWhatTheInterpreterCannot(t *testing.T) {
	t.Parallel()

	h := testHost(t) // built before the clock starts — see the CPU-bound test

	start := time.Now()
	resp, err := h.Run(context.Background(),
		`/(a+)+\1b$/.test('a'.repeat(40))`,
		Limits{Wall: time.Second, CPU: 5 * time.Minute, Memory: 256 << 20})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != RefusalWallClock {
		t.Fatalf("a regex bomb under a 1s wall bound and a 5m CPU bound gave %+v after %s, want %s",
			resp, elapsed.Round(time.Millisecond), RefusalWallClock)
	}
	// It must be the PARENT's message, not the child's: the child's interrupt
	// cannot preempt this class, so a refusal that came from the child would
	// mean the premise changed.
	if !strings.Contains(resp.Message, "outlived its") {
		t.Errorf("the refusal did not come from the parent's deadline: %q", resp.Message)
	}
	// The message names the operator's bound, not the internal grace.
	if !strings.Contains(resp.Message, "1s wall bound") {
		t.Errorf("the refusal does not name the bound that was set: %q", resp.Message)
	}
	if elapsed > 20*time.Second {
		t.Errorf("the parent's deadline took %s to fire", elapsed.Round(time.Second))
	}
}

// A preemptible runaway is the easy arm, and claw's own timeout test covers
// only this one. The discriminator is the CHILD's own wording: an earlier
// version matched "interrupted", which the PARENT's message also contains, so
// deleting the child's interrupt entirely left this test green at 7 s.
func TestAPreemptibleLoopIsStoppedByTheInterpreter(t *testing.T) {
	t.Parallel()

	h := testHost(t) // built before the clock starts — see the CPU-bound test

	start := time.Now()
	resp, err := h.Run(context.Background(), `for(;;){}`,
		Limits{Wall: 2 * time.Second, CPU: time.Minute, Memory: 256 << 20})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != RefusalWallClock {
		t.Fatalf("a busy loop gave %+v, want %s", resp, RefusalWallClock)
	}
	if !strings.Contains(resp.Message, "script interrupted after") {
		t.Errorf("the refusal came from the parent's deadline, not the child's interrupt: %q", resp.Message)
	}
	// And it was stopped by the CHILD at ~2 s, not by the parent at 2 s +
	// wallGrace. Without this bound the wording check alone would pass on a
	// child whose interrupt never fired.
	if elapsed > 2*time.Second+wallGrace/2 {
		t.Errorf("the loop ran %s, which is the parent's deadline rather than the child's interrupt",
			elapsed.Round(time.Millisecond))
	}
}

// A memory bomb dies on RLIMIT_DATA, and the parent names the bound from what
// the Go runtime printed. RLIMIT_AS could not express this: a Go child does
// not start below about 2 GiB of address space, which is above any sane heap
// budget. The ADR listed the two as interchangeable; they are not.
func TestAMemoryBombIsKilledByTheMemoryBound(t *testing.T) {
	t.Parallel()

	const bomb = `(function(){var a=[]; for(var i=0;i<4000000;i++) a.push('xxxxxxxxxxxxxxxx'+i); return a.length})()`
	h := testHost(t)

	resp, err := h.Run(context.Background(), bomb,
		Limits{Memory: 128 << 20, CPU: 2 * time.Minute, Wall: time.Minute})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != RefusalMemory {
		t.Fatalf("a memory bomb gave %+v, want %s", resp, RefusalMemory)
	}

	// And where the runtime does NOT name a cause, the verdict says so instead
	// of guessing. Measured on the shipping binary: the same bomb prints
	// `runtime: out of memory` under 128 MiB and dies with
	// `SIGSEGV: segmentation violation` under 64 MiB. Calling the second one
	// memory_exceeded would be a guess wearing a verdict — and calling it a
	// host error would blame the deployment for a script's appetite.
	resp, err = h.Run(context.Background(), bomb,
		Limits{Memory: 64 << 20, CPU: 2 * time.Minute, Wall: time.Minute})
	if err != nil {
		t.Fatalf("a crash under a tight bound was reported as a broken deployment: %v", err)
	}
	if resp.Refusal != RefusalMemory && resp.Refusal != RefusalHostDied {
		t.Fatalf("a bomb under a 64 MiB bound gave %+v, want %s or the honest %s",
			resp, RefusalMemory, RefusalHostDied)
	}
	if resp.Refusal == RefusalHostDied && !strings.Contains(resp.Message, "memory bound") {
		t.Errorf("the ambiguous verdict does not name the bound a reader would check: %q", resp.Message)
	}
}

// The CALLER's deadline is the caller's, and saying otherwise fabricated a
// number: a 400 ms caller deadline on a script with a 5-minute wall bound used
// to report "killed after 5m5s" and advise bounding CPU time instead. It is an
// error, not a refusal — the script was never given its chance.
func TestTheCallersDeadlineIsNotTheScriptsWallBound(t *testing.T) {
	t.Parallel()

	// The host comes FIRST. realBinary blocks on a shared build on first use,
	// and a deadline created before it was already expired by the time Run
	// was called — so exec.CommandContext failed at Start and the error came
	// from there. The test passed with the branch it means to exercise
	// deleted.
	h := testHost(t)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	_, err := h.Run(ctx, `for(;;){}`, Limits{Wall: 5 * time.Minute, CPU: time.Minute})
	if err == nil {
		t.Fatal("the caller's expired deadline produced a refusal instead of an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the error does not carry the caller's cause: %v", err)
	}
	// And it must come from the DEATH path, not from a failed Start: a Start
	// that never happened proves nothing about how a dead child is read.
	if strings.Contains(err.Error(), "starting") {
		t.Errorf("the child was never started, so this test exercises exec.CommandContext "+
			"rather than the caller-context branch: %v", err)
	}
}

// A cancellation is neither a script refusal nor a broken deployment, and the
// parent knows which it is. It used to come back as script_host_died — the one
// code a caller must read as "this deployment cannot run scripts".
func TestACancellationIsNeitherARefusalNorAHostDeath(t *testing.T) {
	t.Parallel()

	h := testHost(t) // before the context — see the caller-deadline test

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	resp, err := h.Run(ctx, `for(;;){}`, Limits{Wall: time.Minute, CPU: time.Minute})
	if err == nil {
		t.Fatalf("a cancelled run produced %+v instead of an error", resp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the error does not carry context.Canceled, so a caller cannot tell a cancellation "+
			"from a broken deployment: %v", err)
	}
}

// Oversized before the spawn: measuring it in the child would mean buffering
// it there first, and a script nobody will run should not cost a process.
func TestAnOversizedScriptNeverReachesAChild(t *testing.T) {
	t.Parallel()

	// A deliberately unusable binary: if the host spawned anything, this test
	// fails with a host error instead of a refusal.
	h := Host{Binary: "/nonexistent/iterion"}
	resp, err := h.Run(context.Background(), strings.Repeat("x", 2048), Limits{ScriptBytes: 1024})
	if err != nil {
		t.Fatalf("the host tried to spawn: %v", err)
	}
	if resp.Refusal != RefusalScriptBytes {
		t.Fatalf("gave %+v, want %s", resp, RefusalScriptBytes)
	}
}

// The CHILD enforces the script bound too. The parent's pre-spawn refusal
// makes this unreachable through Host — and the sub-command's promise that
// "every bound travels inside the request, where the child re-clamps it" was
// false all the same: Clamp re-clamps the limit VALUES and nothing compared
// the script's length against them.
func TestTheChildEnforcesTheScriptBoundItself(t *testing.T) {
	t.Parallel()

	resp := childWithRequest(t, Request{
		Script:                       strings.Repeat("x", 200),
		Limits:                       Limits{ScriptBytes: 16},
		AllowSingleThreadConfinement: true,
	})
	if resp.Refusal != RefusalScriptBytes {
		t.Errorf("the child accepted a script over its own bound: %+v", resp)
	}
}

// And the child re-clamps the VALUES against the platform ceiling, which is
// what makes a crafted request unable to widen what a tenant was granted. A
// mutant that dropped the child's Clamp left the suite green.
func TestTheChildReclampsAgainstThePlatformCeiling(t *testing.T) {
	t.Parallel()

	// A request asking for far more result than the ceiling allows, with a
	// script that returns more than the ceiling but less than the request.
	resp := childWithRequest(t, Request{
		Script:                       `'x'.repeat(` + strconv.FormatInt(Ceiling().ResultBytes+1<<20, 10) + `)`,
		Limits:                       Limits{ResultBytes: Ceiling().ResultBytes * 8, Memory: Ceiling().Memory},
		AllowSingleThreadConfinement: true,
	})
	if resp.Refusal != RefusalResultBytes {
		t.Errorf("the child served a result bound above the platform ceiling: %+v", resp)
	}
}

// The posture verdict's CALL SITE. The function has a unit test, but removing
// the CALL left the whole suite green: every other arm either uses the
// shipping CGO_ENABLED=0 binary (which confines every thread) or passes the
// allowance.
//
// It reads the posture the child REPORTS and asserts the verdict that posture
// earns, because which side the environment lands on is not this test's to
// choose: a cgo-linked test binary cannot drive its own threads and must be
// refused, while on a runner where it can — measured on this repo's ARC
// runners, where the same binary reported AllThreads=true — the script runs.
// Asserting one of the two would have been asserting the machine.
func TestTheVerdictMatchesThePostureTheChildReports(t *testing.T) {
	t.Parallel()

	// With the allowance, the child always runs and always reports.
	vouched := childWithRequest(t, Request{
		Script:                       "1+1",
		AllowSingleThreadConfinement: true,
		AllowWithoutLandlock:         true,
	})
	if vouched.Confinement == nil {
		t.Fatal("the child reported no confinement, so its posture is unobservable")
	}
	weak := !vouched.Confinement.AllThreads || vouched.Confinement.LandlockABI == 0

	// Without it, the verdict must follow that posture.
	plain := childWithRequest(t, Request{Script: "1+1"})
	switch {
	case weak && plain.Refusal != RefusalUnavailable:
		t.Errorf("the child's posture is weak (%+v) and it ran the script anyway: %+v",
			vouched.Confinement, plain)
	case weak && !strings.Contains(plain.Message, "allow_"):
		t.Errorf("the refusal does not name an escape hatch: %q", plain.Message)
	case !weak && !plain.OK():
		t.Errorf("the child confines every thread under a Landlock ruleset (%+v) and refused anyway: %+v",
			vouched.Confinement, plain)
	}
}

// A child that cannot be started is a HOST failure, not a script refusal. The
// two must not collapse: a caller that retried a refusal would retry a broken
// deployment forever.
func TestAMissingBinaryIsAHostErrorNotARefusal(t *testing.T) {
	t.Parallel()

	_, err := Host{Binary: "/nonexistent/iterion"}.Run(context.Background(), "1", Limits{})
	if err == nil {
		t.Fatal("a missing binary produced no error")
	}
	if !strings.Contains(err.Error(), Subcommand) {
		t.Errorf("the error does not say what was being started: %v", err)
	}
}

// And neither is a deployment that answers something else. These came back as
// script_host_died — a verdict about the script — while the cause was a
// rolling deploy or a resolver that found an older binary.
func TestABrokenDeploymentIsAHostErrorNotARefusal(t *testing.T) {
	t.Parallel()

	for name, script := range map[string]string{
		"not iterion at all":      `echo "I am not iterion" >&2; exit 127`,
		"no such sub-command":     `echo 'Error: unknown command "__script-host"' >&2; exit 1`,
		"a wrapper that swallows": `exit 0`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := fakeChild(t, script).Run(context.Background(), "1", Limits{})
			if err == nil {
				t.Error("a deployment that answered nothing produced a refusal instead of an error")
			}
		})
	}
}

// The parent's own stdout cap. Before it existed, 256 MiB of child stdout grew
// the parent by 1 GiB of total allocation — in a package whose premise is what
// a compromised child cannot do.
func TestTheParentBoundsWhatItBuffersFromTheChild(t *testing.T) {
	t.Parallel()

	h := fakeChild(t, `head -c 8388608 /dev/zero | tr '\0' 'x'; exit 0`)
	resp, err := h.Run(context.Background(), "1", Limits{ResultBytes: 64 << 10})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != RefusalResultBytes {
		t.Fatalf("a flood of stdout gave %+v, want %s", resp, RefusalResultBytes)
	}
}

// One stray byte on the stream used to discard a complete, valid verdict: a
// descendant writing a single character turned a value into "produced no
// response".
func TestAStrayByteDoesNotDiscardTheVerdict(t *testing.T) {
	t.Parallel()

	h := fakeChild(t, `printf '{"value":"42"}\n'; printf 'x'; exit 0`)
	resp, err := h.Run(context.Background(), "1", Limits{})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if !resp.OK() || resp.Value != "42" {
		t.Fatalf("gave %+v, want the value 42", resp)
	}
}

// No descendant outlives Run. os/exec dispatches the armed Cancel only when
// the CONTEXT fires, so on the ordinary path — the child exits by itself —
// nothing ever signalled the group, and a descendant held the inherited pipes
// for the full drain bound. proc.KillProcessGroup exists for exactly that
// window; it was not called.
func TestNoDescendantOutlivesASuccessfulRun(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "alive")
	h := fakeChild(t, `(while :; do echo x >> `+marker+`; sleep 0.1; done) & printf '{"value":"42"}\n'; exit 0`)

	start := time.Now()
	resp, err := h.Run(context.Background(), "1", Limits{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("refused: %+v", resp)
	}
	// The duration is NOT asserted. os/exec's Wait drains the inherited pipes
	// before it returns, and a descendant holding them costs the wait delay —
	// measured 5.0 s — whatever the group kill does afterwards, because the
	// kill can only run inside the teardown window the primitive's own
	// documentation requires. What matters, and what is asserted, is that
	// nothing of the child is left alive once Run has returned.
	_ = elapsed

	size := func() int64 {
		fi, err := os.Stat(marker)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	before := size()
	time.Sleep(700 * time.Millisecond)
	if after := size(); after > before {
		t.Errorf("a descendant of the child is still running after Run returned (marker grew %d → %d)",
			before, after)
	}
}

// The child's /proc is closed to a same-uid reader once it has confined
// itself: PR_SET_DUMPABLE(0) reassigns /proc/<pid> away from this user, so a
// sibling in the same pod cannot read the script host's memory or its boot
// environment. In the in-pod posture that is the only thing between them.
func TestAConfinedChildsProcIsClosedToASibling(t *testing.T) {
	t.Parallel()

	report := runEscapeHelper(t, raceSafeLimits(Limits{Memory: 256 << 20, CPU: 30 * time.Second, Wall: 30 * time.Second}))
	if !report.procReadableBeforeConfine {
		t.Fatal("the probe could not read the child's /proc BEFORE it confined itself, so this arm " +
			"cannot tell PR_SET_DUMPABLE from an environment that hides /proc from everyone")
	}
	if report.procReadableBySibling {
		t.Error("a same-uid neighbour read the confined child's boot environment through /proc: " +
			"PR_SET_DUMPABLE did not take, and in the in-pod posture that is the only thing between " +
			"a script host and the runner's memory")
	}
}

// childWithRequest runs a hand-built request through a real child, for the
// child-side decisions the parent's own clamping makes unreachable through
// Host.
//
// It SPAWNS rather than calling RunChild in-process, and that is not a
// preference: Confine is irreversible and process-wide, so an in-process call
// confines the test binary itself. Measured the one time it was written that
// way — every later exec in the suite failed with
// `fork/exec /usr/local/go/bin/go: permission denied`, including the build of
// the artefact the bound arms need.
//
// The child here is this test binary (see TestMain), which is cgo-linked and
// can therefore confine only one thread; these arms are about the child's
// ARITHMETIC, and the process-wide guarantee is measured against the shipping
// artefact in TestTheRealBinaryConfinesEveryThreadOfTheProcess.
func childWithRequest(t *testing.T, req Request) Response {
	t.Helper()

	// The child here is this test binary, so under -race it carries
	// ThreadSanitizer and needs its floor before it runs a line.
	req.Limits = raceSafeLimits(req.Limits)

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], Subcommand)
	cmd.Stdin = strings.NewReader(string(payload))
	cmd.Env = []string{}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v | stdout: %q | stderr: %q", err, stdout.String(), stderr.String())
	}
	resp, ok := decodeResponse([]byte(stdout.String()))
	if !ok {
		t.Fatalf("the child produced no verdict: %q (stderr %q)", stdout.String(), stderr.String())
	}
	return resp
}
