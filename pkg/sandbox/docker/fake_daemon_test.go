package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The driver shells out to a container CLI instead of embedding a daemon
// client, so a stand-in binary set as the driver's [Runtime] exercises the
// PUBLIC driver path — Prepare, Start, Exec, Cleanup — with fault injection
// and no daemon at all (the same stand-in technique image_env_test.go's
// fakeInspector uses for `image inspect`). Every invocation is appended
// verbatim to a log file, so the tests below assert what the driver ASKED
// the daemon to do: the docker run argv with the workspace bind-mount, the
// conflict retry, the exec, and the idempotent stop+rm cleanup.

const (
	fakeDaemonContainerID = "deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcdef"
)

// fakeDaemon writes the stand-in runtime script and hands back the Runtime
// path plus the log file the script appends to. Behaviour knobs are files
// the script reads (set them with t.Setenv):
//
//   - FAKE_DAEMON_CONFLICT=once|always — `run` fails first (once) or every
//     (always) time with the daemon's container-name-conflict message.
//   - FAKE_DAEMON_IMAGE_MISSING=1 — `image inspect` exits 1, so Prepare
//     must pull.
func fakeDaemon(t *testing.T) (Runtime, string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("the stand-in daemon is a shell script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "invocations.log")
	conflicted := filepath.Join(dir, "conflicted")
	bin := filepath.Join(dir, "fake-runtime")
	script := "#!/bin/sh\n" +
		"{ printf '%s\\n' '---'; printf '%s\\n' \"$@\"; } >> \"$FAKE_DAEMON_LOG\"\n" +
		"case \"$1\" in\n" +
		"  image)\n" +
		"    [ \"$FAKE_DAEMON_IMAGE_MISSING\" = 1 ] && exit 1\n" +
		"    exit 0 ;;\n" +
		"  pull) exit 0 ;;\n" +
		"  run)\n" +
		"    if [ -n \"$FAKE_DAEMON_CONFLICT\" ] && { [ \"$FAKE_DAEMON_CONFLICT\" = always ] || [ ! -f \"$FAKE_DAEMON_CONFLICTED\" ]; }; then\n" +
		"      [ \"$FAKE_DAEMON_CONFLICT\" = once ] && : > \"$FAKE_DAEMON_CONFLICTED\"\n" +
		"      printf '%s\\n' 'docker: Error response from daemon: Conflict. The container name \"/iterion-run_x\" is already in use by container \"cafebabe123456\". You have to remove (or rename) that container to be able to reuse that name.' >&2\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    printf '%s\\n' \"$FAKE_DAEMON_CONTAINER_ID\"\n" +
		"    ;;\n" +
		"  inspect) printf '%s\\n' \"${FAKE_DAEMON_PID_MODE:-private}\" ;;\n" +
		"  exec) printf '%s\\n' 'exec-ran-in-container' ;;\n" +
		"  stop) exit 0 ;;\n" +
		"  rm) exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DAEMON_LOG", log)
	t.Setenv("FAKE_DAEMON_CONFLICTED", conflicted)
	t.Setenv("FAKE_DAEMON_CONTAINER_ID", fakeDaemonContainerID)
	// The persistent /nix volume keys on `image inspect --format {{.Id}}`,
	// one more daemon round-trip that would blur the argv under test; the
	// operator's opt-out is the documented way to keep it out.
	t.Setenv("ITERION_SANDBOX_PERSIST_NIX", "0")
	return Runtime(bin), log
}

// invocations reads the stand-in's log back as one argv per invocation.
func invocations(t *testing.T, log string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read stand-in daemon log: %v", err)
	}
	var out [][]string
	var cur []string
	for _, line := range strings.Split(string(raw), "\n") {
		switch line {
		case "---":
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = nil
		case "":
		default:
			cur = append(cur, line)
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// subcommand returns the daemon's first argv word (`run`, `exec`, `stop`, …).
func subcommand(inv []string) string { return inv[0] }

// argvOf returns the last invocation whose first word is cmd; the test
// fails when the daemon saw none.
func argvOf(t *testing.T, log, cmd string) []string {
	t.Helper()
	var found []string
	for _, inv := range invocations(t, log) {
		if subcommand(inv) == cmd {
			found = inv
		}
	}
	if found == nil {
		t.Fatalf("the stand-in daemon never saw a %q invocation; log: %v", cmd, invocations(t, log))
	}
	return found
}

// fakeDriver binds a fresh discard-logged driver to the stand-in daemon.
func fakeDriver(t *testing.T, rt Runtime) *Driver {
	return &Driver{rt: rt, logger: discardLogger()}
}

// lifecycleSpec is the spec whose translation into docker run argv the
// lifecycle test pins: an explicit WorkspaceFolder so source and target of
// the workspace bind-mount visibly differ, and TWO env pairs so the argv's
// sorted-key order is a claim the test can fail on rather than a vacuous
// one.
func lifecycleSpec() sandbox.Spec {
	return sandbox.Spec{
		Mode:            sandbox.ModeInline,
		Image:           "alpine:test",
		WorkspaceFolder: "/repo",
		User:            "1000:1000",
		Env:             map[string]string{"ITERION_TEST_MARKER": "hello", "ITERION_TEST_MARKER_B": "bye"},
		Mounts:          []string{"type=bind,src=/host/extra,dst=/extra"},
		Tmpfs:           []string{"/tmp/scratch:size=64m"},
	}
}

func TestTheStandInDaemonSeesTheContainersLifecycleAndItsWorkspaceBindMount(t *testing.T) {
	rt, log := fakeDaemon(t)
	d := fakeDriver(t, rt)
	ctx := context.Background()

	prepared, err := d.Prepare(ctx, lifecycleSpec())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ws := t.TempDir()
	run, err := d.Start(ctx, prepared, sandbox.RunInfo{RunID: "run_lifecycle", WorkspacePath: ws})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The docker run argv: the lifecycle contract (a detached, auto-removed,
	// named, labelled long-lived container) and the workspace bind-mount
	// (the HOST tree the run reviews lands INSIDE the container at the
	// spec's workspace folder).
	runArgv := argvOf(t, log, "run")
	wantPrefix := []string{
		"run", "--detach", "--rm",
		"--name", "iterion-run_lifecycle",
		"--label", "iterion.io/managed=true",
		"--label", "iterion.io/run-id=run_lifecycle",
		"--workdir", "/repo",
		"--mount", "type=bind,source=" + ws + ",target=/repo",
	}
	if len(runArgv) < len(wantPrefix) {
		t.Fatalf("docker run argv shorter than the lifecycle contract:\n got=%v", runArgv)
	}
	for i, want := range wantPrefix {
		if runArgv[i] != want {
			t.Fatalf("docker run argv[%d] = %q, want %q\n got=%v", i, runArgv[i], want, runArgv)
		}
	}
	for _, want := range [][]string{
		{"--mount", "type=bind,src=/host/extra,dst=/extra"},
		{"--user", "1000:1000"},
		{"--tmpfs", "/tmp/scratch:size=64m"},
	} {
		if !argvContains(runArgv, want) {
			t.Errorf("docker run argv must carry %v; got=%v", want, runArgv)
		}
	}
	// The spec's env pairs ride the argv in SORTED key order — the
	// determinism envArgs promises. This RunInfo carries neither a proxy
	// nor the host-gateway alias, so every `--env` on the argv is a spec
	// pair and the slice pins exactly; un-sorting envArgs turns this red.
	if got, want := envPairsOfArgs(runArgv), []string{"ITERION_TEST_MARKER=hello", "ITERION_TEST_MARKER_B=bye"}; !slices.Equal(got, want) {
		t.Errorf("docker run env pairs = %v, want the spec's pairs in sorted key order %v", got, want)
	}
	if !argvHasSuffix(runArgv, []string{"alpine:test", "sleep", "infinity"}) {
		t.Errorf("the container must be the long-lived target: the argv ends in the image plus `sleep infinity`; got=%v", runArgv)
	}

	// Start returns the handle the daemon answered with, and the process
	// namespace it read back. (Run.Driver() echoes the runtime the driver
	// was bound to — here the stand-in's own path.)
	dockerRun, ok := run.(*Run)
	if !ok {
		t.Fatalf("Start returned %T, want *Run", run)
	}
	if dockerRun.containerID != fakeDaemonContainerID {
		t.Errorf("container id = %q, want what the daemon answered", dockerRun.containerID)
	}
	if !dockerRun.ProcessIsolated() {
		t.Error("a container whose PidMode the daemon answered `private` must be reported process-isolated")
	}

	// Exec drives the started container: docker exec against the daemon's
	// container id, workdir defaulting to the bind-mount target.
	res, err := run.Exec(ctx, []string{"echo", "hello"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	execArgv := argvOf(t, log, "exec")
	wantExec := []string{"exec", "--workdir", "/repo", "--user", "1000:1000", fakeDaemonContainerID, "echo", "hello"}
	if strings.Join(execArgv, "\x1f") != strings.Join(wantExec, "\x1f") {
		t.Errorf("docker exec argv = %v, want %v", execArgv, wantExec)
	}
	if !strings.Contains(string(res.Stdout), "exec-ran-in-container") {
		t.Errorf("Exec stdout = %q, want the stand-in's marker", res.Stdout)
	}

	// Cleanup stops then force-removes, and a second Cleanup is a no-op.
	if err := run.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	stopArgv := argvOf(t, log, "stop")
	if strings.Join(stopArgv, "\x1f") != strings.Join([]string{"stop", "--time", "5", fakeDaemonContainerID}, "\x1f") {
		t.Errorf("docker stop argv = %v, want a 5s grace stop of the container", stopArgv)
	}
	rmArgv := argvOf(t, log, "rm")
	if strings.Join(rmArgv, "\x1f") != strings.Join([]string{"rm", "--force", fakeDaemonContainerID}, "\x1f") {
		t.Errorf("docker rm argv = %v, want a forced removal of the container", rmArgv)
	}
	after := len(invocations(t, log))
	if err := run.Cleanup(ctx); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if got := len(invocations(t, log)); got != after {
		t.Errorf("a second Cleanup must be idempotent: %d - %d extra daemon calls", after, got-after)
	}
}

func TestAStaleContainerNameConflictIsForceRemovedAndTheStartRetriedOnce(t *testing.T) {
	rt, log := fakeDaemon(t)
	d := fakeDriver(t, rt)
	ctx := context.Background()

	t.Setenv("FAKE_DAEMON_CONFLICT", "once")
	prepared, err := d.Prepare(ctx, lifecycleSpec())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(ctx, prepared, sandbox.RunInfo{RunID: "run_conflict", WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v (a stale container name must be force-removed and retried, not fail the run)", err)
	}

	var runs, removes [][]string
	for _, inv := range invocations(t, log) {
		switch subcommand(inv) {
		case "run":
			runs = append(runs, inv)
		case "rm":
			removes = append(removes, inv)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("the stand-in daemon saw %d run invocations, want the refused one plus the retry", len(runs))
	}
	if strings.Join(runs[0], "\x1f") != strings.Join(runs[1], "\x1f") {
		t.Errorf("the retry must replay the IDENTICAL argv.\n first =%v\n retry=%v", runs[0], runs[1])
	}
	if len(removes) != 1 || strings.Join(removes[0], "\x1f") != strings.Join([]string{"rm", "-f", "iterion-run_conflict"}, "\x1f") {
		t.Errorf("the conflict must force-remove the stale container by NAME, saw rm invocations %v", removes)
	}
	if dr, ok := run.(*Run); !ok || dr.containerID != fakeDaemonContainerID {
		t.Errorf("the retried Start must return the daemon's container, got %#v", run)
	}
}

func TestAThoroughlyConflictedNameFailsTheStartWithoutSilentRecovery(t *testing.T) {
	rt, log := fakeDaemon(t)
	d := fakeDriver(t, rt)
	ctx := context.Background()

	t.Setenv("FAKE_DAEMON_CONFLICT", "always")
	prepared, err := d.Prepare(ctx, lifecycleSpec())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	_, err = d.Start(ctx, prepared, sandbox.RunInfo{RunID: "run_conflict", WorkspacePath: t.TempDir()})
	if err == nil {
		t.Fatal("a name another container holds through the retry must fail the Start, not pass as started")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("the operator must see the daemon's own cause, got: %v", err)
	}
	var runs int
	for _, inv := range invocations(t, log) {
		if subcommand(inv) == "run" {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("the stand-in daemon saw %d run invocations, want exactly the refused one plus one retry", runs)
	}
}

func TestPrepareProbesTheImageAndPullsOnlyWhenTheDaemonLacksIt(t *testing.T) {
	rt, log := fakeDaemon(t)
	d := fakeDriver(t, rt)
	ctx := context.Background()

	// The daemon answers `image inspect` present: no pull may happen.
	if _, err := d.Prepare(ctx, lifecycleSpec()); err != nil {
		t.Fatalf("Prepare with a present image: %v", err)
	}
	for _, inv := range invocations(t, log) {
		if subcommand(inv) == "pull" {
			t.Fatalf("an image the daemon holds must not be pulled; log: %v", invocations(t, log))
		}
	}

	// The daemon lacks the image: Prepare probes, then pulls, in that
	// order — the slow IO stays ahead of any container allocation.
	t.Setenv("FAKE_DAEMON_IMAGE_MISSING", "1")
	if _, err := d.Prepare(ctx, lifecycleSpec()); err != nil {
		t.Fatalf("Prepare with a missing image: %v", err)
	}
	var order []string
	for _, inv := range invocations(t, log) {
		order = append(order, subcommand(inv))
	}
	// The log still holds the first Prepare's probe; the second must have
	// appended inspect-then-pull, and nothing may precede the pull inside
	// that Prepare.
	if len(order) < 3 || order[len(order)-2] != "image" || order[len(order)-1] != "pull" {
		t.Errorf("Prepare must probe the daemon then pull the absent image, got order %v", order)
	}
	pullArgv := argvOf(t, log, "pull")
	if strings.Join(pullArgv, "\x1f") != strings.Join([]string{"pull", "alpine:test"}, "\x1f") {
		t.Errorf("docker pull argv = %v, want the spec's exact image ref", pullArgv)
	}
}

// argvContains reports whether want appears as a contiguous block in argv.
func argvContains(argv, want []string) bool {
	for off := 0; off+len(want) <= len(argv); off++ {
		match := true
		for i := range want {
			if argv[off+i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// envPairsOfArgs reads the `NAME=value` halves of an argv's `--env` pairs,
// in argv order — the argv is the producer, so this is the order the
// daemon actually received.
func envPairsOfArgs(argv []string) []string {
	var pairs []string
	for i, arg := range argv {
		if arg == "--env" && i+1 < len(argv) {
			pairs = append(pairs, argv[i+1])
		}
	}
	return pairs
}

// argvHasSuffix reports whether want appears as a contiguous block at the
// END of argv (the image + command the daemon must run).
func argvHasSuffix(argv, want []string) bool {
	if len(argv) < len(want) {
		return false
	}
	off := len(argv) - len(want)
	for i := range want {
		if argv[off+i] != want[i] {
			return false
		}
	}
	return true
}
