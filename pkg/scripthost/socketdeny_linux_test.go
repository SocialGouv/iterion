//go:build linux

package scripthost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// evalSeccomp runs a classic-BPF seccomp program on one (arch, nr) pair. It
// implements exactly the opcodes socketDenyFilter emits and FAILS on any other,
// so a builder change that emits something new cannot be silently misread.
func evalSeccomp(t *testing.T, prog []unix.SockFilter, arch, nr uint32) uint32 {
	t.Helper()
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			switch in.K {
			case 0:
				acc = nr
			case 4:
				acc = arch
			default:
				t.Fatalf("pc %d loads seccomp_data offset %d, which this evaluator does not model", pc, in.K)
			}
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if acc == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			if acc >= in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_RET | unix.BPF_K:
			return in.K
		default:
			t.Fatalf("pc %d: opcode %#x is not one this evaluator models", pc, in.Code)
		}
	}
	t.Fatalf("the program fell off its end for arch %#x nr %d: no verdict", arch, nr)
	return 0
}

const denyEACCES = unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES)

// Every architecture the release ships, evaluated on whatever host runs this:
// the arm64 half is otherwise never executed in CI.
func TestTheSocketFilterProgram(t *testing.T) {
	for goarch, spec := range socketDenySpecs {
		t.Run(goarch, func(t *testing.T) {
			prog := socketDenyFilter(spec)
			for _, nr := range spec.denied {
				if got := evalSeccomp(t, prog, spec.arch, nr); got != denyEACCES {
					t.Errorf("denied syscall %d answers %#x, want ERRNO|EACCES", nr, got)
				}
				// The neighbours of every denied number stay open: an
				// off-by-one jump would deny a syscall the child needs.
				for _, near := range []uint32{nr - 1, nr + 1} {
					if contains(spec.denied, near) {
						continue
					}
					if got := evalSeccomp(t, prog, spec.arch, near); got != unix.SECCOMP_RET_ALLOW {
						t.Errorf("syscall %d, next to a denied one, answers %#x, want ALLOW", near, got)
					}
				}
			}
			// What the child does run on: read, write, exit and the
			// confinement calls themselves (seccomp, landlock_restrict_self).
			for _, nr := range []uint32{0, 1, 60, 63, 64, 93, 98, 222, 231, 277, 317, 446} {
				if contains(spec.denied, nr) {
					continue
				}
				if got := evalSeccomp(t, prog, spec.arch, nr); got != unix.SECCOMP_RET_ALLOW {
					t.Errorf("syscall %d answers %#x, want ALLOW: this filter is one wall, not an allowlist", nr, got)
				}
			}
			// Another architecture's numbering is a bypass, not a call to
			// judge: the process dies.
			for _, other := range []uint32{unix.AUDIT_ARCH_I386, unix.AUDIT_ARCH_ARM, unix.AUDIT_ARCH_X86_64, unix.AUDIT_ARCH_AARCH64} {
				if other == spec.arch {
					continue
				}
				if got := evalSeccomp(t, prog, other, spec.denied[0]); got != unix.SECCOMP_RET_KILL_PROCESS {
					t.Errorf("a syscall under arch %#x answers %#x, want KILL_PROCESS", other, got)
				}
			}
			// Keyed on the ARCHITECTURE, not on spec.x32: a spec that lost its
			// x32 flag is exactly the defect, and an expectation read from the
			// same field would skip the check it is meant to make.
			if goarch == "amd64" {
				for _, nr := range []uint32{spec.denied[0], 0} {
					if got := evalSeccomp(t, prog, spec.arch, x32SyscallBit|nr); got != unix.SECCOMP_RET_KILL_PROCESS {
						t.Errorf("x32 syscall %d answers %#x, want KILL_PROCESS", nr, got)
					}
				}
			}
		})
	}
}

func contains(xs []uint32, x uint32) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// The literal numbers in socketDenySpecs against the kernel's own tables as
// x/sys vendors them, for EVERY architecture — not only the one compiling.
func TestTheSocketFilterNumbersAreTheKernels(t *testing.T) {
	want := []string{"SYS_SOCKET", "SYS_SOCKETPAIR", "SYS_IO_URING_SETUP", "SYS_IO_URING_ENTER", "SYS_IO_URING_REGISTER"}
	for goarch, spec := range socketDenySpecs {
		src, err := os.ReadFile(filepath.Join("..", "..", "vendor", "golang.org", "x", "sys", "unix",
			"zsysnum_linux_"+goarch+".go"))
		if err != nil {
			t.Fatalf("%s: the vendored syscall table is unreadable: %v", goarch, err)
		}
		if len(spec.denied) != len(want) {
			t.Fatalf("%s: %d denied numbers, want %d (%v)", goarch, len(spec.denied), len(want), want)
		}
		for i, name := range want {
			m := regexp.MustCompile(`(?m)^\s+` + name + `\s+=\s+(\d+)$`).FindSubmatch(src)
			if m == nil {
				t.Fatalf("%s: %s is not in the vendored table", goarch, name)
			}
			n, _ := strconv.ParseUint(string(m[1]), 10, 32)
			if uint32(n) != spec.denied[i] {
				t.Errorf("%s: %s is %d in the kernel's table, %d in the filter", goarch, name, n, spec.denied[i])
			}
		}
	}
}

// TestX32HelperProcess is not a test: confined, it issues socket(2) through
// the x32 ABI, which the filter must answer by killing the process.
func TestX32HelperProcess(t *testing.T) {
	if os.Getenv("ITERION_SCRIPTHOST_X32_HELPER") != "1" {
		t.Skip("helper process only")
	}
	if _, err := Confine(raceSafeLimits(Limits{Memory: 256 << 20, CPU: 30 * time.Second})); err != nil {
		fmt.Println("X32-CONFINE-ERR", err)
		os.Exit(3)
	}
	_, _, errno := unix.RawSyscall(uintptr(x32SyscallBit|unix.SYS_SOCKET), unix.AF_INET, unix.SOCK_DGRAM, 0)
	fmt.Println("X32-SURVIVED", int(errno))
	os.Exit(0)
}

// The arch guard, live. Without it the kernel would treat the x32 number under
// its own table (where the kernel lacks x32, ENOSYS) and the process would go
// on — a filter that compares numbers alone is bypassed through another ABI.
func TestAnX32SyscallKillsTheConfinedChild(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x32 is an amd64 ABI")
	}
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestX32HelperProcess$", "-test.count=1")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ITERION_SCRIPTHOST_X32_HELPER=1"}
	out, err := cmd.CombinedOutput()
	if bytes.Contains(out, []byte("X32-CONFINE-ERR")) {
		t.Fatalf("the helper could not confine itself: %s", out)
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if err == nil || !ok || !ws.Signaled() || ws.Signal() != syscall.SIGSYS {
		t.Fatalf("an x32 socket(2) did not kill the confined child with SIGSYS (err=%v): %s", err, out)
	}
}

// The filter denies CREATING a socket; an inherited one would walk around it,
// so the real script host refuses to run with one.
func TestTheScriptHostRefusesAnInheritedSocket(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("a listener to inherit: %v", err)
	}
	defer ln.Close()
	sock, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("the listener's descriptor: %v", err)
	}
	defer sock.Close()

	payload, err := json.Marshal(Request{Script: "1+1", Limits: DefaultLimits(), Proto: Proto})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(realBinary(t), Subcommand)
	cmd.Env = []string{}
	cmd.Stdin = bytes.NewReader(payload)
	cmd.ExtraFiles = []*os.File{sock}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the script host did not answer (%v): %s", err, out)
	}
	resp, ok := decodeResponse(out)
	if !ok {
		t.Fatalf("no response document: %s", out)
	}
	if resp.Refusal != RefusalUnavailable || !bytes.Contains([]byte(resp.Message), []byte("inherited a socket on descriptor 3")) {
		t.Fatalf("a script host holding an inherited socket ran or refused for another reason: %+v", resp)
	}
}

// The same spawn with no socket runs: the refusal above is the socket's, not
// the hand-built spawn's.
func TestTheScriptHostRunsWithoutAnInheritedSocket(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(Request{Script: "1+1", Limits: DefaultLimits(), Proto: Proto})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(realBinary(t), Subcommand)
	cmd.Env = []string{}
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the script host did not answer (%v): %s", err, out)
	}
	resp, ok := decodeResponse(out)
	if !ok || resp.Value != "2" {
		t.Fatalf("the plain spawn did not run the script: ok=%v %+v", ok, resp)
	}
	if resp.Confinement == nil || !resp.Confinement.SocketsDenied {
		t.Fatalf("the shipped child did not report the socket wall: %+v", resp.Confinement)
	}
}
