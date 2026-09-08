//go:build linux

package proctest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	mode := os.Getenv("ITERION_PROCTEST_FIXTURE")
	if mode == "" {
		os.Exit(m.Run())
	}
	os.Exit(NoProcessLeaks(func() int {
		if lifetime := os.Getenv("ITERION_PROCTEST_LIFETIME"); lifetime != "" {
			// Intentionally orphan a helper after its launcher exits; the suite
			// guard must find it even though the immediate parent has disappeared.
			// Its lifetime picks the side of the settle boundary under test: one
			// that outlives the window is a leak, one that exits inside it is
			// forgiven.
			c := exec.Command("sh", "-c", `"$1" "$3" </dev/null >/dev/null 2>&1 & echo $! > "$2"`, "sh", os.Getenv("ITERION_PROCTEST_HELPER"), os.Getenv("ITERION_PROCTEST_PIDFILE"), lifetime)
			if err := c.Run(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 8
			}
		}
		if strings.HasSuffix(mode, "failure") {
			return 7
		}
		return 0
	}))
}

func TestNoProcessLeaks(t *testing.T) {
	// Both sides of the settle boundary, made explicit: `settle` orphans a
	// helper that exits well inside a widened window (forgiven, silent, still
	// reaped), the `orphan` pair one that outlives a narrowed one (reported).
	// Without the `settle` row the guard could latch on first sight — the
	// behaviour this table exists to forbid — and stay green.
	for _, tc := range []struct {
		mode     string
		code     int
		settle   string // ITERION_PROCTEST_SETTLE; empty leaves the default
		lifetime string // seconds the orphaned helper sleeps; empty spawns none
		leak     bool
	}{
		{mode: "clean", code: 0},
		{mode: "failure", code: 7},
		{mode: "settle", code: 0, settle: "10s", lifetime: "1"},
		{mode: "orphan", code: 1, settle: "100ms", lifetime: "600", leak: true},
		{mode: "orphan-failure", code: 7, settle: "100ms", lifetime: "600", leak: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			pidfile := filepath.Join(dir, "child.pid")
			// Preserve argv[0] basename: Nix coreutils is a multicall binary.
			helper := filepath.Join(dir, "sleep")
			sleep, err := exec.LookPath("sleep")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sleep, helper); err != nil {
				t.Fatal(err)
			}
			// Even a deliberately disabled guard can be canaried without leaving
			// the fixture's own sleep running after this assertion fails.
			t.Cleanup(func() {
				data, _ := os.ReadFile(pidfile)
				pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
				if pid <= 0 {
					return
				}
				if p, err := os.FindProcess(pid); err == nil {
					// Match this fixture's unique argv before signalling a pidfd;
					// an already-reaped PID could have been reused by another session.
					argv, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
					if strings.HasPrefix(string(argv), helper+"\x00") {
						_ = p.Kill()
					}
					_ = p.Release()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
			c.Env = append(os.Environ(), "ITERION_PROCTEST_FIXTURE="+tc.mode, "ITERION_PROCTEST_PIDFILE="+pidfile,
				"ITERION_PROCTEST_HELPER="+helper, "ITERION_PROCTEST_LIFETIME="+tc.lifetime, settleEnv+"="+tc.settle)
			out, err := c.CombinedOutput()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("fixture: %v (%s)", err, out)
				}
				code = exit.ExitCode()
			}
			if code != tc.code {
				t.Fatalf("exit=%d want %d: %s", code, tc.code, out)
			}
			if reported := strings.Contains(string(out), "test process survived suite cleanup"); reported != tc.leak {
				t.Fatalf("leak reported=%v want %v: %s", reported, tc.leak, out)
			}
			if tc.lifetime != "" && !tc.leak {
				// A forgiven child is silent by design, so without this count
				// the case would pass identically to `clean` even with the
				// settle logic deleted.
				if !strings.Contains(string(out), "forgave 1 settling child(ren)") {
					t.Fatalf("settling child not observed then forgiven: %s", out)
				}
			}
			if tc.lifetime != "" {
				data, err := os.ReadFile(pidfile)
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := processState(pid); !os.IsNotExist(err) {
					t.Fatalf("fixture process %d was not reclaimed: %v", pid, err)
				}
			}
		})
	}
}
