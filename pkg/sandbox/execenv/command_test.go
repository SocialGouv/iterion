package execenv

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

func TestNewCommandIsPureCopiesAndReservesSDKStdin(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux host transport only")
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-created")
	l := Launch{HostHelper: missing, ClientPath: "/captured/client", ClientArgs: []string{"arg"}, ControlEnv: []string{"PATH=/control"}, Invocation: sampleLaunch().Invocation, StartTimeout: 30 * time.Second, WaitDelay: 2 * time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	cmd := NewCommand(ctx, l, nil)
	if cmd.Err != nil || cmd.Process != nil || cmd.Stdin != nil || len(cmd.Args) != 2 || cmd.Args[1] != HostMode {
		t.Fatalf("impure/invalid constructor: %#v", cmd)
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 0 {
		t.Fatalf("factory side effect: %v %v", entries, e)
	}
	pipe, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	defer pipe.Close()
	l.ClientArgs[0] = "changed"
	l.ControlEnv[0] = "changed"
	l.Invocation.Env["PATH"] = "changed"
	captured, clean, e := readWireEnvironment(cmd.Env)
	if e != nil || captured.ClientArgs[0] != "arg" || captured.Invocation.Env["PATH"] != "workload-only" || clean[0] != "PATH=/control" {
		t.Fatalf("not captured: %#v %#v %v", captured, clean, e)
	}
	cancel()
	if e = cmd.Start(); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel before Start: %v", e)
	}
	if _, e = os.Stat(missing); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("created helper")
	}
}
func TestNewCommandTypedRefusalBeforeStart(t *testing.T) {
	t.Parallel()
	cases := map[string]Launch{
		"relative-helper":  {HostHelper: "iterion", ClientPath: "/client", Invocation: sampleLaunch().Invocation},
		"relative-client":  {HostHelper: "/iterion", ClientPath: "kubectl", Invocation: sampleLaunch().Invocation},
		"invalid-env":      {HostHelper: "/iterion", ClientPath: "/client", ControlEnv: []string{wireCount + "=evil"}, Invocation: sampleLaunch().Invocation},
		"negative-timeout": {HostHelper: "/iterion", ClientPath: "/client", Invocation: sampleLaunch().Invocation, StartTimeout: -1},
	}
	for name, l := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := NewCommand(t.Context(), l, nil)
			var e *protocol.Error
			if !errors.As(cmd.Err, &e) || !errors.Is(cmd.Start(), protocol.ErrTransport) || cmd.Process != nil {
				t.Fatalf("refusal not carried: %v", cmd.Err)
			}
		})
	}
}
func TestTryMainUnknownModesNeverFallThrough(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"iterion", "__exec-env-host-v2"}, {"iterion", HostMode, "extra"}, {"iterion", "__exec-env-guest-v1"}} {
		var stdout, stderr strings.Builder
		handled, code := TryMain(args, strings.NewReader("unused"), &stdout, &stderr)
		if !handled || code != 125 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "host: mode") {
			t.Fatalf("mode accepted/fell through: %v %v %d %q %q", args, handled, code, stdout.String(), stderr.String())
		}
	}
	handled, _ := TryMain([]string{"iterion", "version"}, nil, io.Discard, io.Discard)
	if handled {
		t.Fatal("ordinary CLI intercepted")
	}
}
