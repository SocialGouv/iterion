//go:build linux

package scripthost

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The witnesses below run this test binary as a child that builds iterion
// through the real realBinary path, with a stand-in `go` first on PATH and a
// scratch TMPDIR, and look at what the build left there.
const (
	buildChildEnv     = "ITERION_SCRIPTHOST_BUILD_CHILD"
	buildChildHoldEnv = "ITERION_SCRIPTHOST_BUILD_CHILD_HOLD"
	buildChildMarker  = "scripthost-build-child: build runs"

	fakeGoLogEnv     = "ITERION_SCRIPTHOST_FAKE_GO_LOG"
	fakeGoFailEnv    = "ITERION_SCRIPTHOST_FAKE_GO_FAIL"
	fakeGoForeignEnv = "ITERION_SCRIPTHOST_FAKE_GO_FOREIGN"
	fakeGoHoldEnv    = "ITERION_SCRIPTHOST_FAKE_GO_HOLD"
	fakeGoStateEnv   = "ITERION_SCRIPTHOST_FAKE_GO_STATE"
	fakeGoOutput     = "stand-in build\n"
)

// fakeGo stands in for `go build -o <path> …`: it logs <path>, then fails, or
// writes the build — and, when asked, a second file next to it, which a build
// that returns never leaves. Asked to hold, it says it started, waits for its
// release file, and only then moves a build into <path>, as cmd/go moves its
// link output out of its work directory. It writes the build itself: it
// models cmd/go, not the linker cmd/go forks.
const fakeGo = `#!/bin/sh
printf '%s\n' "$3" >> "$` + fakeGoLogEnv + `"
if [ -n "$` + fakeGoFailEnv + `" ]; then
	echo "stand-in build failed" >&2
	exit 1
fi
if [ -n "$` + fakeGoHoldEnv + `" ]; then
	printf 'started\n' >> "$` + fakeGoStateEnv + `"
	while [ ! -e "$` + fakeGoHoldEnv + `" ]; do sleep 0.05; done
	printf 'stand-in build\n' > "$` + fakeGoStateEnv + `.out"
	mv "$` + fakeGoStateEnv + `.out" "$3"
	exit 0
fi
printf 'stand-in build\n' > "$3"
if [ -n "$` + fakeGoForeignEnv + `" ]; then
	printf 'left by the build\n' > "$(dirname "$3")/$` + fakeGoForeignEnv + `"
fi
`

// TestBuildChildProcess is the child of the witnesses below. It builds through
// realBinary, checks that the path it got still reads the build, says so, and
// then, when asked, holds until its parent kills it.
func TestBuildChildProcess(t *testing.T) {
	if os.Getenv(buildChildEnv) != "1" {
		t.Skip("helper process only")
	}
	built := realBinary(t)
	got, err := os.ReadFile(built)
	if err != nil || string(got) != fakeGoOutput {
		t.Fatalf("the build path %q does not read the build (%q, %v)", built, got, err)
	}
	// The pin is close-on-exec: no child of this process, the confined script
	// host included, inherits a descriptor on the build.
	fds, err := exec.Command("ls", "/proc/self/fd").Output()
	if err != nil {
		t.Fatalf("listing a child's descriptors: %v", err)
	}
	pinned := strings.TrimPrefix(built, "/proc/self/fd/")
	for _, fd := range strings.Fields(string(fds)) {
		if fd == pinned {
			t.Fatalf("a child inherited the pinned descriptor %s on the build", fd)
		}
	}
	fmt.Println(buildChildMarker)
	if os.Getenv(buildChildHoldEnv) == "1" {
		_, _ = bufio.NewReader(os.Stdin).ReadString(0)
	}
}

// buildChild is one child run of TestBuildChildProcess.
type buildChild struct {
	cmd *exec.Cmd
	tmp string // the child's TMPDIR
	log string // the stand-in's log of `-o` paths
}

func newBuildChild(t *testing.T, env ...string) buildChild {
	t.Helper()
	scratch := t.TempDir()
	bin := filepath.Join(scratch, "bin")
	tmp := filepath.Join(scratch, "tmp")
	for _, dir := range []string{bin, tmp} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(scratch, "go.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestBuildChildProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		buildChildEnv+"=1",
		fakeGoLogEnv+"="+log,
		"TMPDIR="+tmp,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.Env = append(cmd.Env, env...)
	return buildChild{cmd: cmd, tmp: tmp, log: log}
}

// buildOutputs returns the paths the stand-in was asked to build, each of
// which must lie in the child's TMPDIR: a build made elsewhere is one this
// test cannot see the remains of.
func (c buildChild) buildOutputs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(c.log)
	if err != nil {
		t.Fatalf("the stand-in `go` never ran, so what the child left proves nothing: %v", err)
	}
	var outputs []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			outputs = append(outputs, line)
		}
	}
	for _, out := range outputs {
		if rel, err := filepath.Rel(c.tmp, out); err != nil || !filepath.IsLocal(rel) {
			t.Fatalf("the child built %q outside its TMPDIR %q", out, c.tmp)
		}
	}
	return outputs
}

// leftovers lists the build directories present in the child's TMPDIR.
func (c buildChild) leftovers(t *testing.T) []string {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(c.tmp, buildDirPattern+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// TestBuildLeavesNothingWhileTheRunLives kills the child with SIGKILL while it
// still holds its build. Nothing catches SIGKILL and nothing runs after it, so
// a build still on disk at that moment stays there: the build must already be
// gone, while the child can still run it.
func TestBuildLeavesNothingWhileTheRunLives(t *testing.T) {
	c := newBuildChild(t, buildChildHoldEnv+"=1")
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	c.cmd.Stderr = &stderr
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// childOut is written by the reader goroutine only, and read only after
	// a receive from reached.
	var childOut strings.Builder
	reached := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			childOut.WriteString(scanner.Text() + "\n")
			if scanner.Text() == buildChildMarker {
				reached <- true
				return
			}
		}
		reached <- false
	}()
	select {
	case ok := <-reached:
		if !ok {
			_ = c.cmd.Wait()
			t.Fatalf("the child ended without running its build:\n%s%s", childOut.String(), stderr.String())
		}
	case <-time.After(time.Minute):
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		<-reached
		t.Fatalf("the child did not report its build within a minute:\n%s%s", childOut.String(), stderr.String())
	}

	c.buildOutputs(t)
	alive := c.leftovers(t)
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = c.cmd.Wait()
	if len(alive) != 0 {
		t.Fatalf("the build is still on disk while the child runs it, so a SIGKILL leaves it behind: %v", alive)
	}
	if left := c.leftovers(t); len(left) != 0 {
		t.Fatalf("the killed child left build directories behind: %v", left)
	}
}

// TestFailedBuildLeavesNothing makes the build fail: the child fails saying
// so, and its build directory is gone.
func TestFailedBuildLeavesNothing(t *testing.T) {
	c := newBuildChild(t, fakeGoFailEnv+"=1")
	out, err := c.cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the child passed although its build failed:\n%s", out)
	}
	if !strings.Contains(string(out), "building iterion with CGO_ENABLED=0") {
		t.Fatalf("the child failed without naming the build:\n%s", out)
	}
	c.buildOutputs(t)
	if left := c.leftovers(t); len(left) != 0 {
		t.Fatalf("the failed build left its directory behind: %v\n%s", left, out)
	}
}

// TestBuildDirWithAStrangerFails leaves a second file next to the build. The
// run removes its own build, keeps the file it did not write, and fails
// naming it, rather than passing over a leak or deleting what it did not make.
func TestBuildDirWithAStrangerFails(t *testing.T) {
	const stranger = "iterion-go-tmp-umask"
	c := newBuildChild(t, fakeGoForeignEnv+"="+stranger)
	out, err := c.cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the child passed although its build directory still holds %q:\n%s", stranger, out)
	}
	if !strings.Contains(string(out), stranger) {
		t.Fatalf("the child failed without naming %q:\n%s", stranger, out)
	}
	outputs := c.buildOutputs(t)
	if len(outputs) != 1 {
		t.Fatalf("the stand-in ran %d builds, want 1: %v", len(outputs), outputs)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outputs[0]), stranger)); err != nil {
		t.Fatalf("the run deleted a file it did not write (%v):\n%s", err, out)
	}
	if _, err := os.Stat(outputs[0]); !os.IsNotExist(err) {
		t.Fatalf("the run kept its own build %q (stat: %v):\n%s", outputs[0], err, out)
	}
}

// TestBuildKilledMidwayLeavesNoBuild kills the child with SIGKILL while cmd/go
// still runs, then releases the stand-in. A cmd/go that outlives the run moves
// its output into a directory nobody is left to remove, so it must die with
// the run. The stand-in's empty directory may stay: nothing ran after the
// SIGKILL to remove it (a real cmd/go also leaves its work directory, see
// buildIterion).
func TestBuildKilledMidwayLeavesNoBuild(t *testing.T) {
	scratch := t.TempDir()
	release := filepath.Join(scratch, "release")
	state := filepath.Join(scratch, "state")
	c := newBuildChild(t, fakeGoHoldEnv+"="+release, fakeGoStateEnv+"="+state)
	// The child and its build inherit the write end: reading to EOF waits
	// until both are gone.
	gone, held, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gone.Close()
	c.cmd.ExtraFiles = []*os.File{held}
	var out strings.Builder
	c.cmd.Stdout, c.cmd.Stderr = &out, &out
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	held.Close()

	deadline := time.Now().Add(time.Minute)
	for {
		if raw, _ := os.ReadFile(state); strings.Contains(string(raw), "started") {
			break
		}
		if time.Now().After(deadline) {
			_ = c.cmd.Process.Kill()
			_ = c.cmd.Wait()
			t.Fatalf("the stand-in build never started:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = c.cmd.Wait()
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, gone)
		ended <- err
	}()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the stand-in build still runs a minute after its release")
	}

	c.buildOutputs(t)
	builds, err := filepath.Glob(filepath.Join(c.tmp, buildDirPattern+"*", buildOutput))
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 0 {
		t.Fatalf("a run killed while its build ran left the build behind: %v", builds)
	}
}
