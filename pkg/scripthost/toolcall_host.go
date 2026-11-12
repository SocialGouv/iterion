package scripthost

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// toolChannel is the parent's end of the pair handed to the child.
//
// The child's ends are closed in the parent as soon as the spawn has taken
// them, which is what makes an EOF on calls mean "the child is gone" rather
// than "nobody has written yet": a copy of the write end left open in the
// parent would keep the read end alive forever and the pump would block past
// the child's death.
type toolChannel struct {
	replies *os.File // the parent WRITES results here; the child reads fd 3
	calls   *os.File // the parent READS calls here; the child writes fd 4

	childReplies *os.File
	childCalls   *os.File
}

func newToolChannel() (*toolChannel, error) {
	// fd 3: child reads, parent writes.
	rRep, wRep, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("scripthost: opening the tool channel's reply pipe: %w", err)
	}
	// fd 4: child writes, parent reads.
	rCall, wCall, err := os.Pipe()
	if err != nil {
		_ = rRep.Close()
		_ = wRep.Close()
		return nil, fmt.Errorf("scripthost: opening the tool channel's call pipe: %w", err)
	}
	return &toolChannel{replies: wRep, calls: rCall, childReplies: rRep, childCalls: wCall}, nil
}

// attach hands the child its two descriptors. ORDER IS THE PROTOCOL:
// cmd.ExtraFiles numbers them from 3, so replies must come first.
func (c *toolChannel) attach(cmd *exec.Cmd) {
	cmd.ExtraFiles = []*os.File{c.childReplies, c.childCalls}
}

// closeChildEnds drops the parent's copies of what the child now owns. Called
// right after Start, by the caller, because before Start the child has not
// taken them yet.
func (c *toolChannel) closeChildEnds() {
	_ = c.childReplies.Close()
	_ = c.childCalls.Close()
}

func (c *toolChannel) close() {
	_ = c.replies.Close()
	_ = c.calls.Close()
}

// serveVerdict is what the pump observed. It travels as ONE value handed back
// by the goroutine's join, not as fields a reader assembles from two channels:
// this repo has been bitten by a fact reconstructed from the order of a
// concurrent stream, and "did the channel fail, and how far had it got" is
// exactly that kind of fact.
type serveVerdict struct {
	served int64
	err    error
}

// pump runs the parent's half of the tool channel on its own goroutine and
// returns a join function.
//
// The pump must run CONCURRENTLY with cmd.Wait: the child blocks on its first
// call until the parent answers, and a parent that waited for the child first
// would deadlock on the very first tool a script used. The join returns the
// verdict, so the caller reads one value after Wait rather than racing the
// goroutine for two.
func pump(ctx context.Context, c *toolChannel, srv ToolServer, limits Limits) func() serveVerdict {
	var (
		once sync.Once
		out  serveVerdict
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		served, err := serveToolCalls(ctx, c.calls, c.replies, srv, limits)
		out = serveVerdict{served: served, err: err}
	}()

	return func() serveVerdict {
		// Closing the parent's end of the CALL pipe is what ends a pump still
		// blocked in a read after the child died: the child's write end went
		// with the process, but a pump parked on a read needs the fd shut to
		// see EOF when the kill raced it.
		once.Do(func() {
			_ = c.calls.Close()
			<-done
		})
		return out
	}
}
