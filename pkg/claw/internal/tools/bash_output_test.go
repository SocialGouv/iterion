package tools

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/permissions"
)

func TestBashOutputRetainsOnlyBoundedPrefix(t *testing.T) {
	var output bashOutput
	chunk := []byte(strings.Repeat("x", 4096))
	for range 4096 { // drain 16 MiB without retaining it
		n, err := output.Write(chunk)
		if err != nil || n != len(chunk) {
			t.Fatalf("must drain the complete write: n=%d error=%v", n, err)
		}
	}
	if output.size != maxOutputSize || !output.truncated {
		t.Fatalf("capture size=%d truncated=%v", output.size, output.truncated)
	}
	want := strings.Repeat("x", maxOutputSize) + "\n... [output truncated]"
	if got := output.String(); got != want {
		t.Fatalf("wrong retained prefix: length=%d", len(got))
	}
}

func TestBashOutputExactLimitIsNotTruncated(t *testing.T) {
	var output bashOutput
	want := strings.Repeat("x", maxOutputSize)
	_, _ = output.Write([]byte(want))
	_, _ = output.Write(nil)
	if output.truncated || output.String() != want {
		t.Fatal("the exact limit is complete output, not a truncation")
	}
}

// TestExecuteBashNeverCarriesTheWholeOutput exercises the SITE, not the
// writer. The returned string cannot tell the two implementations apart — a
// buffer that accumulates everything and truncates after cmd.Run returns the
// same bytes as one that never kept them. What differs is the memory the run
// holds while the command is still printing, so that is what is asserted:
// drive far more output than the limit through the real path and require the
// heap not to have carried it. Restoring a growing bytes.Buffer at the call
// site turns this red; nothing else here does.
func TestExecuteBashNeverCarriesTheWholeOutput(t *testing.T) {
	const produced = 16 << 20 // 16 MiB, ~1600x the retained prefix

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	out, err := ExecuteBash(
		context.Background(),
		map[string]any{"command": "yes x | head -c " + strconv.Itoa(produced)},
		permissions.ModeAllow, "",
	)
	runtime.ReadMemStats(&after)

	if err != nil {
		t.Fatalf("command must succeed: %v", err)
	}
	want := strings.Repeat("x\n", maxOutputSize/2) + "\n... [output truncated]"
	if out != want {
		t.Fatalf("returned output is not the bounded prefix: length=%d", len(out))
	}

	// TotalAlloc is cumulative and survives the collection of a buffer that
	// has already gone out of scope by the time ExecuteBash returns — which
	// a live-heap reading would miss entirely. A growing buffer costs at
	// least `produced` (and ~2x it while doubling); the bounded writer costs
	// the fixed array plus os/exec's own 32 KiB copy buffer.
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > produced/4 {
		t.Fatalf("the run carried the output: allocated %d bytes for %d bytes of output", allocated, produced)
	}
}

// TestBoundedOutputCapturesBothStreamsWithoutRacing pins the assumption the
// bounded writer's safety rests on, which nothing else exercises.
//
// bashOutput has no lock. It is safe only because cmd.Stdout and cmd.Stderr
// are the SAME pointer: os/exec's childStderr returns childStdout when
// interfaceEqual(Stderr, Stdout) holds, so the child gets one pipe and one
// goroutine copies it — there are never two concurrent Writes to race on.
//
// Give the two streams separate writers and that argument evaporates silently:
// the output still looks plausible, and only a reader who knows to look finds
// the interleaved corruption. Under `go test -race` this test is what turns
// that edit red.
func TestBoundedOutputCapturesBothStreamsWithoutRacing(t *testing.T) {
	const lines = 200

	out, err := ExecuteBash(
		context.Background(),
		map[string]any{"command": "for i in $(seq 1 " + strconv.Itoa(lines) + "); do echo out; echo err 1>&2; done"},
		permissions.ModeAllow, "",
	)
	if err != nil {
		t.Fatalf("command must succeed: %v", err)
	}

	// Both streams reached the same buffer — a writer wired to stdout only
	// would drop every `err` line and still return something that reads fine.
	gotOut := strings.Count(out, "out")
	gotErr := strings.Count(out, "err")
	if gotOut != lines || gotErr != lines {
		t.Fatalf("captured %d stdout and %d stderr lines, want %d of each — the two streams do not share the buffer", gotOut, gotErr, lines)
	}
	// Every byte written is one of the two whole lines. A torn write (two
	// goroutines copying into the array at once) shows up here as a line that
	// is neither, which counting alone would miss.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "out" && line != "err" {
			t.Fatalf("output carries a torn line %q — the writes were not serialized", line)
		}
	}
}
