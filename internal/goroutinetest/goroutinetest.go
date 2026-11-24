// Package goroutinetest identifies the live goroutines a test is answerable
// for by the FRAME they run, read out of the runtime's own dump.
//
// A worker that owns its goroutine — production closes nothing and publishes
// nothing when it returns — leaves a test no other oracle, and two witnesses
// of this tree draw a leak verdict from it: the runs service's background stop
// (pkg/runview) and the server's project hot-swap (pkg/server). The rule that
// decides whether a goroutine CARRIES a frame lives here so both read the same
// one: a frame matched as a bare substring counts a sibling whose name carries
// it as a PREFIX, which turns a leak verdict into a pass, and a copy of the
// rule fixed at one witness leaves the other holding the hole.
//
// Dump is the other half: every reading here takes the runtime's dump whole,
// and a test whose mark is looser than a frame takes it from here too.
package goroutinetest

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ByFrame returns, per label, the ids of the goroutines whose OWN stack
// carries that label's frame. Every label comes out of ONE stop-the-world
// snapshot, so the readings describe the same instant and cost one dump; a
// label nothing carries gets an empty set, never a missing one.
//
// A frame matches as frame+"(": the runtime prints every frame of a dump with
// its argument list, and a bare substring also matches a sibling whose name
// carries the frame as a prefix — startPipelineScheduler.func1 inside
// startPipelineScheduler.func11, the name the runtime hands the worker as soon
// as a tenth literal is written above it in the same function.
//
// The reading is defined on the goroutine's own stack, which stops at the
// "created by" line: that line names the frame of the goroutine's PARENT, so a
// worker's children would carry it without being workers. The runtime prints
// it without an argument list, so frame+"(" already excludes it — the cut
// keeps the reading defined on the own stack rather than on that spelling.
//
// The runtime never reuses a goroutine id, so two readings are comparable as
// SETS, which is the shape a leak verdict needs: a COUNT cannot tell "this
// service's worker is gone" from "it is alive and an ambient one left in
// between", and a goroutine the first reading does not hold and the second
// does was started in between, whatever the rest of the process did meanwhile.
func ByFrame(t *testing.T, frames map[string]string) map[string]map[int]bool {
	t.Helper()
	byLabel := make(map[string]map[int]bool, len(frames))
	for label := range frames {
		byLabel[label] = map[int]bool{}
	}
	for _, block := range strings.Split(Dump(), "\n\n") {
		head, stack, ok := strings.Cut(block, "\n")
		if !ok || !strings.HasPrefix(head, "goroutine ") {
			continue
		}
		own, _, _ := strings.Cut(stack, "\ncreated by ")
		var carries []string
		for label, frame := range frames {
			if strings.Contains(own, frame+"(") {
				carries = append(carries, label)
			}
		}
		if len(carries) == 0 {
			continue
		}
		id := idOf(t, head)
		for _, label := range carries {
			byLabel[label][id] = true
		}
	}
	return byLabel
}

// Carrying returns the ids of the goroutines whose own stack carries frame, by
// the rule ByFrame applies.
func Carrying(t *testing.T, frame string) map[int]bool {
	t.Helper()
	return ByFrame(t, map[string]string{frame: frame})[frame]
}

// initialDumpBuf is the buffer Dump starts from. One reading of this tree's
// heaviest suite, which keeps a few hundred goroutines standing, fits in it;
// the growth loop answers for every reading that does not.
const initialDumpBuf = 1 << 20

// Dump renders every goroutine's stack, growing the buffer until the runtime
// fits in it. runtime.Stack writes what fits and reports only what it wrote,
// so a fixed buffer cuts the dump at its end without a word, and what sits
// past the cut is a goroutine missing from a verdict's set, or a mark a wait
// never reaches before its deadline.
//
// A reading that matches something looser than a frame — a package fragment, a
// function and its closures alike — is its own test's business rather than
// ByFrame's, and needs the dump whole just the same.
func Dump() string {
	buf := make([]byte, initialDumpBuf)
	n := runtime.Stack(buf, true)
	for n == len(buf) {
		buf = make([]byte, 2*len(buf))
		n = runtime.Stack(buf, true)
	}
	return string(buf[:n])
}

// idOf reads the goroutine id out of a dump block's header. Fatal, never a
// skip: a header this reader cannot parse is a member silently missing from the
// set a caller draws its verdict from.
func idOf(t *testing.T, head string) int {
	t.Helper()
	fields := strings.Fields(head)
	if len(fields) < 2 {
		t.Fatalf("unreadable goroutine header %q — this oracle reads ids out of the runtime's dump", head)
	}
	id, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("unreadable goroutine id in %q: %v", head, err)
	}
	return id
}
