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

// isHelper reports whether pid is still THIS fixture's own orphaned helper,
// matched on the unique argv[0] it was symlinked under. A reaped PID belongs
// to the kernel again, so neither the reclaim assertion nor the canary kill
// may act on the number alone.
func isHelper(pid int, helper string) bool {
	argv, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && strings.HasPrefix(string(argv), helper+"\x00")
}

func TestNoProcessLeaksCapabilityBoundary(t *testing.T) {
	for _, stage := range []string{"children-unavailable", "subreaper-unavailable", "scan-failed-after-suite"} {
		for _, suiteCode := range []int{0, 7} {
			t.Run(fmt.Sprintf("%s/suite-exit-%d", stage, suiteCode), func(t *testing.T) {
				calls, enables, scans := 0, 0, 0
				unavailable := errors.New("capability unavailable in this environment")
				got := noProcessLeaks(func() int {
					calls++
					return suiteCode
				}, func() error {
					enables++
					if stage == "subreaper-unavailable" {
						return unavailable
					}
					return nil
				}, func() ([]int, error) {
					scans++
					if stage == "children-unavailable" || (stage == "scan-failed-after-suite" && calls > 0) {
						return nil, unavailable
					}
					return nil, nil
				})
				if calls != 1 {
					t.Fatalf("suite ran %d times, want exactly once even without guard support", calls)
				}
				want := suiteCode
				if stage == "scan-failed-after-suite" && want == 0 {
					want = 1
				}
				if got != want {
					t.Fatalf("exit = %d, want %d", got, want)
				}
				if stage == "children-unavailable" && enables != 0 {
					t.Fatal("adoption was enabled without a usable child scan")
				}
				if stage == "scan-failed-after-suite" && (enables != 1 || scans != 2) {
					t.Fatalf("post-suite scan error was not distinguished from startup: enables=%d scans=%d", enables, scans)
				}
			})
		}
	}
}

// The subprocess table below cannot reach the scan-ended case: it needs a
// child to die inside the microseconds between the guard's state read and its
// per-PID reap, which no fixture can schedule. So the accounting is pinned
// here instead — without the nil-cur row, that child is silently dropped and
// the `settle` fixture's count assertion is flaky rather than wrong.
func TestForgiven(t *testing.T) {
	settling := procKey{pid: 11, start: 100}
	survivor := procKey{pid: 22, start: 200}
	seen := map[procKey]time.Time{settling: {}, survivor: {}}
	reported := map[procKey]bool{survivor: true}
	for _, tc := range []struct {
		name string
		cur  map[procKey]time.Time
		want int
	}{
		{name: "still-alive", cur: seen, want: 0},
		{name: "settling-child-gone", cur: map[procKey]time.Time{survivor: {}}, want: 1},
		// The ECHILD break: no children left at all, so the entry the previous
		// pass reaped after seeing it alive is forgiven here or nowhere.
		{name: "scan-ended", cur: nil, want: 1},
		// A leak already has its verdict; the count is forgiveness, not exits.
		{name: "reported-never-forgiven", cur: map[procKey]time.Time{settling: {}}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := forgiven(seen, tc.cur, reported); got != tc.want {
				t.Fatalf("forgiven = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNoProcessLeaks(t *testing.T) {
	unsupported := false
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
					if isHelper(pid, helper) {
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
			// The clean subprocess is also the capability probe, before any
			// intentional orphan is spawned. Other package suites still run
			// unguarded on this host; only this kernel-specific fixture skips.
			if strings.Contains(string(out), "proctest: leak guard unavailable (") {
				unsupported = true
				return
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
				// A PID that still resolves is not proof on its own: it was
				// reaped, so the kernel is free to have handed it to somebody
				// else since. Only this fixture's own argv makes it ours.
				if _, err := processState(pid); !os.IsNotExist(err) && isHelper(pid, helper) {
					t.Fatalf("fixture process %d was not reclaimed", pid)
				}
			}
		})
		if unsupported {
			t.Skip("kernel does not support the process leak guard")
		}
	}
}
