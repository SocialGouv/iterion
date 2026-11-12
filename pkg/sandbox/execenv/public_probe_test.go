//go:build linux && amd64

package execenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

// This filter is installed in a disposable child on the exact calling thread.
// It makes metadata lookup observable; no filter is installed in the runner.
func trapStatCalls() error {
	runtime.LockOSThread()
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); e != 0 {
		return e
	}
	f := []syscall.SockFilter{{Code: 0x20, K: 0}}
	for _, nr := range []uint32{4, 5, 6, 262, 332} {
		f = append(f, syscall.SockFilter{Code: 0x15, Jt: 0, Jf: 1, K: nr}, syscall.SockFilter{Code: 0x06, K: 0x00030000})
	}
	f = append(f, syscall.SockFilter{Code: 0x06, K: 0x7fff0000})
	program := syscall.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	_, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 22, 2, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	runtime.KeepAlive(f)
	if e != 0 {
		return e
	}
	return nil
}
func TestPurityFilteredConstructorChild(t *testing.T) {
	mode := os.Getenv("K1B_FILTER_CHILD")
	if mode == "" {
		return
	}
	helper := "/definitely/missing/iterion"
	if mode == "relative" {
		helper = "iterion"
	}
	invocation := protocol.Invocation{Argv: []string{"/target"}, Env: map[string]string{"key": "value"}}
	if e := trapStatCalls(); e != nil {
		fmt.Fprintln(os.Stderr, "FILTER_UNAVAILABLE", e)
		os.Exit(90)
	}
	cmd := NewCommand(context.Background(), Launch{HostHelper: helper, ClientPath: "/captured/client", Invocation: invocation}, nil)
	if mode == "relative" && cmd.Err == nil {
		os.Exit(91)
	}
	if mode == "absolute" && cmd.Err != nil {
		os.Exit(92)
	}
	os.Exit(0)
}
func TestPublicConstructorNeverLooksUpRejectedHelper(t *testing.T) {
	self, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"absolute", "relative"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), self, "-test.run=^TestPurityFilteredConstructorChild$")
			cmd.Env = []string{"K1B_FILTER_CHILD=" + mode, "PATH=/definitely/missing", "GOTRACEBACK=none"}
			out, e := cmd.CombinedOutput()
			if e != nil {
				t.Fatalf("CONSTRUCTOR_LOOKUP_IO mode=%s: %v %s", mode, e, out)
			}
		})
	}
}

type observedRefusalWriter struct {
	once sync.Once
	seen chan struct{}
}

func (w *observedRefusalWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.seen) })
	return 0, io.ErrClosedPipe
}
func TestWriterFailureClient(t *testing.T) {
	if os.Getenv("K1B_WRITER_CLIENT") != "1" {
		return
	}
	if _, e := protocol.ReadInvocation(os.Stdin); e != nil {
		os.Exit(91)
	}
	fmt.Fprint(os.Stdout, protocol.Readiness+"payload")
	for {
		time.Sleep(time.Hour)
	}
}
func TestWriterFailurePublicChild(t *testing.T) {
	if os.Getenv("K1B_WRITER_HOST") != "1" {
		return
	}
	w := &observedRefusalWriter{seen: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan struct {
		handled bool
		code    int
	}, 1)
	go func() {
		h, c := TryMain([]string{"iterion", HostMode}, strings.NewReader(""), w, &stderr)
		done <- struct {
			handled bool
			code    int
		}{h, c}
	}()
	select {
	case <-w.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture did not reach output writer")
	}
	forced := false
	var result struct {
		handled bool
		code    int
	}
	select {
	case result = <-done:
	case <-time.After(time.Second):
		forced = true
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		result = <-done
	}
	if forced {
		t.Fatal("OUTPUT_REFUSAL_WAITS_FOR_EXTERNAL_CANCEL: writer refused after real readiness but client remained live")
	}
	if !result.handled || result.code != 125 || !strings.Contains(stderr.String(), "ready: output") {
		t.Fatalf("output refusal lost: %#v %q", result, stderr.String())
	}
}
func TestPublicHostCancelsClientAfterOutputFailure(t *testing.T) {
	self, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := encodeWire(wireLaunch{ClientPath: self, ClientArgs: []string{"-test.run=^TestWriterFailureClient$"}, Invocation: protocol.Invocation{Argv: []string{"/unused"}}, StartTimeout: 3 * time.Second, WaitDelay: 300 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	env, e := wireEnvironment([]string{"K1B_WRITER_HOST=1", "K1B_WRITER_CLIENT=1"}, raw)
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.CommandContext(t.Context(), self, "-test.run=^TestWriterFailurePublicChild$", "-test.timeout=8s")
	cmd.Env = env
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("PUBLIC_OUTPUT_REFUSAL: %v\n%s", e, out)
	}
}
