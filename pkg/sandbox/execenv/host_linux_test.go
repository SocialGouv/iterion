//go:build linux

package execenv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

func TestMain(m *testing.M) {
	if handled, code := TryMain(os.Args, os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// Only the exact subprocess flag runs this passive client fixture. It consumes
// the real frame; workload stdin is never reconstructed from test arguments.
func TestRelayClient(t *testing.T) {
	if os.Getenv("RELAY_CLIENT_FIXTURE") != "1" {
		return
	}
	argv := os.Args
	sep := 0
	for i, a := range argv {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep == 0 || sep+1 >= len(argv) {
		os.Exit(91)
	}
	mode := argv[sep+1]
	if len(argv) > sep+2 {
		if e := os.WriteFile(argv[sep+2], []byte(strconv.Itoa(os.Getpid())), 0600); e != nil {
			os.Exit(92)
		}
	}
	if mode == "silent" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "old-help" {
		fmt.Fprintln(os.Stdout, "usage: old helper --help")
		os.Exit(0)
	}
	in, e := protocol.ReadInvocation(os.Stdin)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(93)
	}
	for _, s := range os.Environ() {
		if strings.HasPrefix(s, wirePrefix) {
			fmt.Fprintln(os.Stderr, "CHUNK_LEAK")
			os.Exit(94)
		}
	}
	if in.Env["PATH"] != "workload-only" || os.Getenv("PATH") != "/control" {
		fmt.Fprintln(os.Stderr, "AUTHORITY_LEAK")
		os.Exit(95)
	}
	fmt.Fprint(os.Stdout, protocol.InvocationReadiness(in))
	switch mode {
	case "early":
		fmt.Fprint(os.Stdout, "early")
		os.Exit(37)
	case "stream":
		fmt.Fprint(os.Stdout, "ACK\n")
		var b [1]byte
		if _, e = io.ReadFull(os.Stdin, b[:]); e != nil {
			os.Exit(96)
		}
		fmt.Fprint(os.Stdout, "byte:")
		_, _ = os.Stdout.Write(b[:])
		os.Exit(0)
	case "exit":
		fmt.Fprint(os.Stdout, "business")
		os.Exit(37)
	case "sleep":
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(97)
	}
}
func fixtureCommand(t *testing.T, mode, pid string, start time.Duration) *exec.Cmd {
	t.Helper()
	self, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	args := []string{"-test.run=^TestRelayClient$", "--", mode}
	if pid != "" {
		args = append(args, pid)
	}
	return NewCommand(t.Context(), Launch{HostHelper: self, ClientPath: self, ClientArgs: args, ControlEnv: []string{"PATH=/control", "RELAY_CLIENT_FIXTURE=1"}, Invocation: sampleLaunch().Invocation, StartTimeout: start, WaitDelay: 300 * time.Millisecond}, nil)
}
func TestRelayExitAndOpenSDKStdin(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"exit", "early"} {
		t.Run(mode, func(t *testing.T) {
			cmd := fixtureCommand(t, mode, "", time.Second)
			in, e := cmd.StdinPipe()
			if e != nil {
				t.Fatal(e)
			}
			defer in.Close()
			var out, stderr bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &stderr
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case e = <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("host waited for SDK stdin EOF")
			}
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 37 || stderr.Len() != 0 {
				t.Fatalf("exit fidelity: %v %q", e, stderr.String())
			}
			want := "business"
			if mode == "early" {
				want = "early"
			}
			if out.String() != want {
				t.Fatalf("output=%q", out.String())
			}
		})
	}
}

type readinessAckWriter struct {
	mu   sync.Mutex
	body bytes.Buffer
	once sync.Once
	ack  chan struct{}
}

func (w *readinessAckWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, e := w.body.Write(b)
	if bytes.HasPrefix(w.body.Bytes(), []byte("ACK\n")) {
		w.once.Do(func() { close(w.ack) })
	}
	return n, e
}
func (w *readinessAckWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}
func TestRelayStartupTimeoutStopsAtReadiness(t *testing.T) {
	t.Parallel()
	cmd := fixtureCommand(t, "stream", "", 300*time.Millisecond)
	in, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	defer in.Close()
	out := &readinessAckWriter{ack: make(chan struct{})}
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	// ACK is a suffix after Readiness, observed through the real host writer.
	// The watchdog is a fixture prerequisite, not evidence about workload time.
	select {
	case <-out.ack:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("startup never reached ACK: %q", stderr.String())
	}
	time.Sleep(700 * time.Millisecond)
	if _, e = in.Write([]byte{0xff}); e != nil {
		t.Fatalf("startup capped acknowledged workload: %v", e)
	}
	if e = cmd.Wait(); e != nil || out.String() != "ACK\nbyte:\xff" || stderr.Len() != 0 {
		t.Fatalf("after acknowledged ready: %v %q %q", e, out.String(), stderr.String())
	}
}
func TestRelayRejectsMissingReadinessAndSilentClient(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"old-help", "silent"} {
		t.Run(mode, func(t *testing.T) {
			cmd := fixtureCommand(t, mode, "", 300*time.Millisecond)
			var out, stderr bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &stderr
			e := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 125 || out.Len() != 0 {
				t.Fatalf("unknown helper accepted: %v %q %q", e, out.String(), stderr.String())
			}
			want := "ready: version"
			if mode == "silent" {
				want = "host: startup-timeout"
			}
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("wrong refusal: %q", stderr.String())
			}
		})
	}
}
func waitPID(t *testing.T, path string) int {
	t.Helper()
	limit := time.After(3 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		b, e := os.ReadFile(path)
		if e == nil {
			n, e := strconv.Atoi(string(b))
			if e != nil {
				t.Fatal(e)
			}
			return n
		}
		select {
		case <-limit:
			t.Fatal("client never started")
		case <-tick.C:
		}
	}
}
func assertDead(t *testing.T, pid int) {
	t.Helper()
	limit := time.After(3 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(e, os.ErrNotExist) || bytes.Contains(b, []byte(") Z ")) {
			return
		}
		select {
		case <-limit:
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("direct client survived host")
		case <-tick.C:
		}
	}
}
func TestRelayControlledCancelAndDirectParentDeath(t *testing.T) {
	t.Parallel()
	for _, hard := range []bool{false, true} {
		t.Run(strconv.FormatBool(hard), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pid")
			cmd := fixtureCommand(t, "sleep", path, time.Second)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if e := cmd.Start(); e != nil {
				t.Fatal(e)
			}
			pid := waitPID(t, path)
			if hard {
				_ = cmd.Process.Kill()
			} else {
				if e := cmd.Cancel(); e != nil {
					t.Fatal(e)
				}
			}
			_ = cmd.Wait()
			assertDead(t, pid)
			// No claim here about a remote pod or an independently escaped descendant.
		})
	}
}
func TestRelayLaunchEnvelopeRecordsExactTimeouts(t *testing.T) {
	t.Parallel()
	cmd := fixtureCommand(t, "exit", "", 0)
	l, _, e := readWireEnvironment(cmd.Env)
	if e != nil || l.StartTimeout != 0 || l.WaitDelay != 300*time.Millisecond {
		t.Fatal("resolved durations changed")
	}
	b, e := json.Marshal(cmd.Args)
	if e != nil || bytes.Contains(b, []byte("workload")) {
		t.Fatal("workload leaked into host argv")
	}
}
