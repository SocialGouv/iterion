//go:build linux

package scripthost

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// The memory bound is the script's ROOM, witnessed on the shipped binary at the
// floor. The shipped child holds ~86 MiB of data segment before the interpreter
// exists (the Go runtime maps a 64 MiB heap arena read-write), so an absolute
// RLIMIT_DATA at the 64 MiB floor started it above its own bound: this very
// script died of a SIGSEGV in the garbage collector 5 runs out of 5 (#3383).
func TestTheMemoryFloorIsRoomForTheScript(t *testing.T) {
	t.Parallel()
	// Ten thousand pushes: a few hundred KiB of heap, real work by any measure.
	const work = `(function(){var a=[]; for(var i=0;i<10000;i++) a.push('x'+i); return a.length})()`
	limits := DefaultLimits()
	limits.Memory = MemoryFloor().Memory
	payload, err := json.Marshal(Request{Script: work, Limits: limits, Proto: Proto})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(realBinary(t), Subcommand)
	cmd.Env = []string{}
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	resp, ok := decodeResponse(out)
	if !ok {
		t.Fatalf("the script host answered no document at the memory floor (%v): %s", runErr, stderr.Bytes())
	}
	if resp.Value != "10000" {
		t.Fatalf("at the memory floor, a script pushing 10 000 strings did not complete: %+v (stderr: %.400s)",
			resp, stderr.Bytes())
	}

	// The bound the kernel holds is the base plus the room, and the child says
	// both: a reader comparing memory_bytes with the 64 MiB it asked for would
	// otherwise see a limit it cannot explain.
	c := resp.Confinement
	if c == nil || c.MemoryBaseBytes == 0 {
		t.Fatalf("the child did not report the data segment its bound is room above: %+v", c)
	}
	var inherited unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_DATA, &inherited); err != nil {
		t.Fatal(err)
	}
	want := min(c.MemoryBaseBytes+uint64(limits.Memory), inherited.Max)
	if c.MemoryBytes != want {
		t.Fatalf("RLIMIT_DATA is %d, want the %d-byte base plus the %d-byte room (%d)",
			c.MemoryBytes, c.MemoryBaseBytes, limits.Memory, want)
	}
}
