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
		if strings.HasPrefix(mode, "orphan") {
			// Intentionally orphan a helper after its launcher exits; the suite
			// guard must find it even though the immediate parent has disappeared.
			c := exec.Command("sh", "-c", `"$1" 600 </dev/null >/dev/null 2>&1 & echo $! > "$2"`, "sh", os.Getenv("ITERION_PROCTEST_HELPER"), os.Getenv("ITERION_PROCTEST_PIDFILE"))
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
	for _, tc := range []struct {
		mode string
		code int
	}{
		{"clean", 0}, {"failure", 7}, {"orphan", 1}, {"orphan-failure", 7},
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
			c.Env = append(os.Environ(), "ITERION_PROCTEST_FIXTURE="+tc.mode, "ITERION_PROCTEST_PIDFILE="+pidfile, "ITERION_PROCTEST_HELPER="+helper)
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
			if strings.HasPrefix(tc.mode, "orphan") {
				if !strings.Contains(string(out), "test process survived suite cleanup") {
					t.Fatalf("missing leak diagnosis: %s", out)
				}
				data, err := os.ReadFile(pidfile)
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := processState(pid); !os.IsNotExist(err) {
					t.Fatalf("leaked fixture process %d still exists: %v", pid, err)
				}
			}
		})
	}
}
