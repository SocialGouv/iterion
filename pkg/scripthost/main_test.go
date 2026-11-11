//go:build linux

package scripthost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

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

// realBinary builds `iterion` the way it ships and returns its path. Built
// once per run; Go's build cache makes the second run of a session cheap
// (measured 19.5 s cold, 3.6 s warm).
func realBinary(t *testing.T) string {
	t.Helper()
	realBinaryOnce.Do(func() {
		realBinaryPath, realBinaryErr = buildIterion("0")
	})
	if realBinaryErr != nil {
		t.Fatalf("the bound arms need the REAL CGO_ENABLED=0 binary and it could not be built, so "+
			"they would otherwise measure a cgo-linked child that cannot confine its own threads: %v", realBinaryErr)
	}
	return realBinaryPath
}

// cgoBinary builds `iterion` WITH cgo, which is the one configuration in which
// a child cannot apply a syscall to its own threads. It exists so the refusing
// branch of the posture verdict is reachable deterministically rather than
// wherever the leg happens to be cgo-linked.
func cgoBinary(t *testing.T) string {
	t.Helper()
	cgoBinaryOnce.Do(func() {
		cgoBinaryPath, cgoBinaryErr = buildIterion("1")
	})
	if cgoBinaryErr != nil {
		t.Fatalf("this arm asserts that a binary which cannot confine its own threads is REFUSED, and "+
			"it needs a cgo-linked build to be such a binary: %v", cgoBinaryErr)
	}
	return cgoBinaryPath
}

// buildIterion builds the CLI with the given CGO_ENABLED and returns its path.
func buildIterion(cgo string) (string, error) {
	dir, err := os.MkdirTemp("", "scripthost-artifact-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "iterion")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/iterion")
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgo)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building iterion with CGO_ENABLED=%s: %w\n%s", cgo, err, out)
	}
	return path, nil
}

// repoRoot is this package's directory's grandparent — pkg/scripthost.
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Dir(filepath.Dir(wd))
}
