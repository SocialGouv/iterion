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
//     threads, so the test binary — which `go test` links WITH cgo — can only
//     confine the thread that asked. Measuring the bounds there would have
//     proved a per-thread fact while the record claims a per-process one.
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
)

// realBinary builds `iterion` the way it ships and returns its path. Built
// once per run; Go's build cache makes the second run of a session cheap
// (measured 19.5 s cold, 3.6 s warm).
func realBinary(t *testing.T) string {
	t.Helper()
	realBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "scripthost-artifact-")
		if err != nil {
			realBinaryErr = err
			return
		}
		realBinaryPath = filepath.Join(dir, "iterion")
		cmd := exec.Command("go", "build", "-o", realBinaryPath, "./cmd/iterion")
		cmd.Dir = repoRoot()
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			realBinaryErr = fmt.Errorf("building the real binary: %w\n%s", err, out)
		}
	})
	if realBinaryErr != nil {
		t.Fatalf("the bound arms need the REAL CGO_ENABLED=0 binary and it could not be built, so "+
			"they would otherwise measure a cgo-linked child that cannot confine its own threads: %v", realBinaryErr)
	}
	return realBinaryPath
}

// repoRoot is this package's directory's grandparent — pkg/scripthost.
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Dir(filepath.Dir(wd))
}
