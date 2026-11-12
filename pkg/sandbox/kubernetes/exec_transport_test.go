package kubernetes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/execenv"
	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

func transportRun(t *testing.T) *Run {
	t.Helper()
	isolateTransportConfig(t)
	// A host environment may not supply the closed relay-control namespace.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ITERION_EXECENV_WIRE_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv(proc.WaitDelayEnv, "2s")
	cfg, err := loadExecTransportConfig()
	if err != nil {
		t.Fatal(err)
	}
	r := testRun()
	r.driver.transport = cfg
	return r
}

// Constructor assertions inspect both real frames through the production leaf
// reader. Native execution is a separate public integration witness, not claimed
// by this test-only envelope observation.
func inspectTransport(t *testing.T, cmd *exec.Cmd) (protocol.Invocation, protocol.Invocation, time.Duration, time.Duration) {
	t.Helper()
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	values := map[string]string{}
	for _, entry := range cmd.Env {
		k, v, _ := strings.Cut(entry, "=")
		values[k] = v
	}
	count, err := strconv.Atoi(values["ITERION_EXECENV_WIRE_COUNT"])
	if err != nil || count < 1 {
		t.Fatalf("missing envelope: %v", err)
	}
	var encoded strings.Builder
	for i := 0; i < count; i++ {
		encoded.WriteString(values["ITERION_EXECENV_WIRE_"+strconv.Itoa(i)])
	}
	raw, err := base64.StdEncoding.DecodeString(encoded.String())
	if err != nil || len(raw) < 24 {
		t.Fatalf("invalid envelope: %v", err)
	}
	if string(raw[:8]) != "IENVHST1" {
		t.Fatalf("unexpected envelope marker: %q", raw[:8])
	}
	reader := bytes.NewReader(raw[24:])
	control, err := protocol.ReadInvocation(reader)
	if err != nil {
		t.Fatal(err)
	}
	in, err := protocol.ReadInvocation(reader)
	if err != nil {
		t.Fatal(err)
	}
	if reader.Len() != 0 {
		t.Fatalf("trailing frame bytes: %d", reader.Len())
	}
	return control, in, time.Duration(binary.BigEndian.Uint64(raw[8:16])), time.Duration(binary.BigEndian.Uint64(raw[16:24]))
}

func TestExecTransportCommandUsesCapturedProfileAndClient(t *testing.T) {
	r := transportRun(t)
	t.Setenv("KUBECONFIG", "operator-config")
	t.Setenv(execTransportPrefix+"HOST_HELPER", "/not-the-captured-helper")
	t.Setenv(execTransportPrefix+"SHELL", "/not-the-captured-shell")
	env := map[string]string{"KUBECONFIG": "target-config", "SHELL": "target-shell", "SECRET": "secret-\xff"}
	argv := []string{"target", "arg-\xff"}
	cmd := r.Command(context.Background(), argv, sandbox.ExecOpts{Env: env, WorkDir: "/other", KeepStdinOpen: true})
	env["SECRET"] = "changed"
	argv[1] = "changed"
	control, in, start, drain := inspectTransport(t, cmd)
	if cmd.Path != "/operator/iterion" || !reflect.DeepEqual(cmd.Args, []string{"/operator/iterion", execenv.HostMode}) {
		t.Fatalf("host authority changed: %q", cmd.Args)
	}
	want := []string{"/usr/local/bin/kubectl", "--namespace", "ns", "exec", "--stdin", "sandbox-x", "--container", "workload", "--", defaultGuestHelper}
	if !reflect.DeepEqual(control.Argv, want) || len(control.Env) != 0 || control.WorkDir != nil || control.Profile != nil {
		t.Fatalf("client control polluted: %+v", control)
	}
	if in.Profile == nil || in.Profile.DefaultPath != "/bin:/usr/bin" || in.Profile.Shell != "/bin/sh" || in.WorkDir == nil || *in.WorkDir != "/other" || in.Env["SECRET"] != "secret-\xff" || in.Argv[1] != "arg-\xff" {
		t.Fatalf("invocation changed: %+v", in)
	}
	if start != 30*time.Second || drain != 2*time.Second || cmd.WaitDelay != drain {
		t.Fatalf("timeouts changed: %v %v %v", start, drain, cmd.WaitDelay)
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("SDK streams already occupied")
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "secret") {
			t.Fatalf("target secret in host argv: %q", arg)
		}
	}
	for _, entry := range cmd.Env {
		if entry == "KUBECONFIG=target-config" || entry == "SHELL=target-shell" {
			t.Fatal("target value became host configuration")
		}
	}
	if !slicesContains(cmd.Env, "KUBECONFIG=operator-config") {
		t.Fatal("host configuration lost")
	}
	pipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = pipe.Close(); err != nil {
		t.Fatal(err)
	}
}
func slicesContains(xs []string, needle string) bool {
	for _, x := range xs {
		if x == needle {
			return true
		}
	}
	return false
}

func TestExecTransportCommandPreservesStreamOwnership(t *testing.T) {
	r := transportRun(t)
	script := strings.Repeat("x", sandbox.MaxInlineArgBytes+1)
	for _, reserved := range []bool{false, true} {
		cmd := r.Command(context.Background(), []string{"bash", "-c", script}, sandbox.ExecOpts{Env: map[string]string{"X": "Y"}, KeepStdinOpen: reserved, WorkDir: "/ws"})
		_, in, _, _ := inspectTransport(t, cmd)
		if in.WorkDir != nil {
			t.Fatal("default directory was made custom")
		}
		if reserved {
			if cmd.Stdin != nil || len(in.Argv) != 3 || in.Argv[1] != "-c" || in.Argv[2] != script {
				t.Fatal("reserved stdin stolen")
			}
			pipe, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			_ = pipe.Close()
		} else {
			if !reflect.DeepEqual(in.Argv, []string{"bash", "-s"}) {
				t.Fatalf("script shell changed: %q", in.Argv)
			}
			b, err := io.ReadAll(cmd.Stdin)
			if err != nil || string(b) != script {
				t.Fatalf("script stdin changed: %v", err)
			}
		}
	}
}

func TestExecTransportCommandDefersConfigRefusalWithoutDowngrade(t *testing.T) {
	r := transportRun(t)
	refusal := execConfigRefusal("protocol", nil)
	r.driver.transportErr = refusal
	required := r.Command(context.Background(), []string{"target"}, sandbox.ExecOpts{Env: map[string]string{"X": "Y"}})
	if required.Err != refusal || !errors.Is(required.Err, protocol.ErrTransport) || required.Path != "" {
		t.Fatalf("required command downgraded: %+v", required)
	}
	for _, env := range []map[string]string{nil, {}} {
		legacy := r.Command(context.Background(), []string{"target"}, sandbox.ExecOpts{Env: env})
		if legacy.Err != nil || legacy.Path != "/usr/local/bin/kubectl" || legacy.Env != nil || slicesContains(legacy.Args, execenv.HostMode) {
			t.Fatalf("legacy command changed: %+v", legacy)
		}
	}
}
