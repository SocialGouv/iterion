//go:build linux

package scripthost

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/SocialGouv/iterion/internal/proctest"
)

// TestMain makes this test binary able to BE the escape helper's child, and
// builds the REAL iterion binary for every arm that measures a bound.
//
// Two children, because they prove different things and one cannot do both:
//
//   - The bound arms and the process-wide confinement claim run against
//     `iterion __script-host` built CGO_ENABLED=0, which is how iterion ships
//     (Dockerfile, Taskfile, release workflow). That matters more than
//     convenience: a cgo-linked binary cannot apply a syscall to its own
//     threads, and whether THIS binary is cgo-linked is a property of the leg
//     (`CGO_ENABLED=0` on CI's unit leg, 1 on its race shards, and whatever a
//     developer's `go test` happens to pick). Measuring the bounds there would
//     have proved a per-thread fact on some legs while the record claims a
//     per-process one.
//   - The escape arms need a child that ATTEMPTS open, connect and exec after
//     confining, which the real binary does not do. That child is this test
//     binary, pinned to its confining thread, with the single-thread
//     allowance set and its reason on the record.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == Subcommand {
		if err := RunChild(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "script-host child:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(proctest.NoProcessLeaks(m.Run))
}

var (
	realBinaryOnce sync.Once
	realBinaryPath string
	realBinaryErr  error

	cgoBinaryOnce sync.Once
	cgoBinaryPath string
	cgoBinaryErr  error
)

// realBinary builds `iterion` the way it ships — CGO_ENABLED=0 and -trimpath
// (Dockerfile, release workflow) — and returns its path. Built once per run.
// -trimpath keeps the checkout path out of the build cache key, so every
// worktree and clone shares this build's cache (measured on a fresh clone:
// 11 s, against 47 s without). In exchange it reuses nothing a plain
// `go test ./...` of the same checkout compiled, and CI's CGO_ENABLED=0 unit
// leg pays one more compile of cmd/iterion for it.
func realBinary(t *testing.T) string {
	t.Helper()
	realBinaryOnce.Do(func() {
		realBinaryPath, realBinaryErr = buildIterion("0", "-trimpath")
	})
	if realBinaryErr != nil {
		t.Fatalf("the bound arms need the REAL CGO_ENABLED=0 binary and it could not be built or pinned, so "+
			"they would otherwise measure a cgo-linked child that cannot confine its own threads: %v", realBinaryErr)
	}
	return realBinaryPath
}

// cgoBinary builds `iterion` WITH cgo, which is the one configuration in which
// a child cannot apply a syscall to its own threads. It exists so the refusing
// branch of the posture verdict is reachable deterministically rather than
// wherever the leg happens to be cgo-linked. It is not the shipped build; it
// is built -trimpath like realBinary so that every checkout shares its cache.
// This package's own test compiles a small part of cmd/iterion's dependencies,
// so a plain build recompiles most of them in each checkout (measured on a
// fresh clone: 38 s, against 3 s). No CI leg compiles a plain cgo build
// without -race, so CI loses nothing.
func cgoBinary(t *testing.T) string {
	t.Helper()
	cgoBinaryOnce.Do(func() {
		cgoBinaryPath, cgoBinaryErr = buildIterion("1", "-trimpath")
	})
	if cgoBinaryErr != nil {
		t.Fatalf("this arm asserts that a binary which cannot confine its own threads is REFUSED, and "+
			"it needs a cgo-linked build to be such a binary: %v", cgoBinaryErr)
	}
	return cgoBinaryPath
}

// TestTheBuildsAreTheOnesTheirArmsClaim reads the build settings recorded in
// each binary: the real one CGO_ENABLED=0 and -trimpath, as the Dockerfile and
// the release workflow build iterion; the cgo one CGO_ENABLED=1, -trimpath for
// the shared cache.
func TestTheBuildsAreTheOnesTheirArmsClaim(t *testing.T) {
	for _, b := range []struct {
		name string
		path string
		want map[string]string
	}{
		{"real", realBinary(t), map[string]string{"CGO_ENABLED": "0", "-trimpath": "true"}},
		{"cgo", cgoBinary(t), map[string]string{"CGO_ENABLED": "1", "-trimpath": "true"}},
	} {
		info, err := buildinfo.ReadFile(b.path)
		if err != nil {
			t.Fatalf("reading the %s binary's build info: %v", b.name, err)
		}
		settings := map[string]string{}
		for _, s := range info.Settings {
			settings[s.Key] = s.Value
		}
		for key, want := range b.want {
			if got := settings[key]; got != want {
				t.Errorf("the %s binary was built with %s=%q, want %q", b.name, key, got, want)
			}
		}
	}
}

const (
	buildDirPattern = "scripthost-artifact-"
	buildOutput     = "iterion"
	// pinnedFDFloor keeps the pin away from the descriptors a fork overwrites
	// before execve: 3, 4, … with a child's ExtraFiles, and those just past
	// the highest descriptor it hands the child with what it moves out of the
	// way. A pin there is lost before the child's execve reads it. The floor
	// keeps the pins far above both in this suite.
	pinnedFDFloor = 256
)

// buildIterion builds the CLI with the given CGO_ENABLED and build flags and
// returns a path that executes it: /proc/self/fd/N, a close-on-exec
// descriptor this process holds on the build. The kernel keeps the file while
// the descriptor is open, and a child forked from this process resolves the
// path in its own execve, before close-on-exec drops the descriptor. That
// holds for an ELF binary, which execve maps whole; a script would not run
// this way, its interpreter reopening the path after the descriptor is gone.
//
// The build's file and directory are removed as soon as the build returns, so
// a run that ends later leaves nothing in TMPDIR, SIGKILL included. A run
// killed while `go build` runs leaves its empty build directory and cmd/go's
// work directory (go-build* in GOTMPDIR, else TMPDIR): cmd/go dies with the
// run, before it removes that directory, which holds every package compiled so
// far, and the compiler and linker it forked outlive it and can go on writing
// there — the linker's whole output when the kill lands during the link. A run
// killed once cmd/go has put the output in place, before the removal, leaves
// the build in its build directory; where cmd/go copies it there instead of
// renaming it (a setgid TMPDIR, a GOTMPDIR on another filesystem), a kill
// during the copy leaves it partial.
func buildIterion(cgo string, flags ...string) (string, error) {
	dir, err := os.MkdirTemp("", buildDirPattern)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, buildOutput)
	args := append([]string{"build", "-o", path}, flags...)
	cmd := exec.Command("go", append(args, "./cmd/iterion")...)
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgo)
	// cmd/go dies with this process. Orphaned, it would finish by moving its
	// output into a directory nobody is left to remove. Pdeathsig reaches
	// cmd/go only, not the compiler or linker it forks. The kernel sends it
	// when the forking THREAD exits (go.dev/issue/27505), so the fork runs on
	// a thread locked for the build's lifetime.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var pinned string
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		err = fmt.Errorf("building iterion with CGO_ENABLED=%s: %w\n%s", cgo, buildErr, out)
	} else {
		pinned, err = pin(path)
	}
	if removeErr := removeBuildDir(dir); removeErr != nil {
		return "", errors.Join(err, removeErr)
	}
	return pinned, err
}

// pin opens path and returns /proc/self/fd/N for a close-on-exec duplicate at
// or above pinnedFDFloor, which stays open for the life of the process.
func pin(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, pinnedFDFloor)
	if err != nil {
		return "", fmt.Errorf("pinning %s at descriptor %d or above, which needs an open-file limit (RLIMIT_NOFILE) above %d: %w",
			path, pinnedFDFloor, pinnedFDFloor, err)
	}
	return fmt.Sprintf("/proc/self/fd/%d", fd), nil
}

// removeBuildDir removes the build output — absent when the build failed —
// then the directory. os.Remove refuses a directory that still holds
// anything: a `go build -o` that returns leaves only its output there, so
// whatever else is left is not this code's to delete, and the error names it.
func removeBuildDir(dir string) error {
	if err := os.Remove(filepath.Join(dir, buildOutput)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(dir); err != nil {
		entries, listErr := os.ReadDir(dir)
		if listErr != nil {
			return fmt.Errorf("%w (listing what is left: %v)", err, listErr)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return fmt.Errorf("%w: it still holds %s, which the build did not write", err, strings.Join(names, ", "))
	}
	return nil
}

// repoRoot is this package's directory's grandparent — pkg/scripthost.
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Dir(filepath.Dir(wd))
}
