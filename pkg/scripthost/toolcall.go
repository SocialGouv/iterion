package scripthost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Proto is the tool-channel protocol version the parent and the child must
// agree on.
//
// It travels in the [Request] rather than in the environment because the
// child's environment is empty by construction. A child that reads a version
// it does not know refuses to run the script at all: a script that believes it
// can call tools, running in a child whose channel the parent does not serve,
// would silently become a script with no capability — and a capability that
// disappears quietly is worse than one that refuses.
//
// The reverse direction cannot refuse: an OLD child ignores an unknown JSON
// field, so it runs the script with no binding and writes a bare [Response].
// That document carries no frame type, which the parent reads as an unknown
// type and reports as a host error — fail-closed, but by the frame check
// rather than by this number. Said here because it is the half a version
// handshake does not cover.
//
// 2: `call.describe` and `call.search`, answered from the request's catalogue
// (CM-3). A version-1 child would run a program that uses them into "not a
// function"; the handshake refuses it as a different build instead.
// 3: `call.parallel` and the batch frame (CM-9).
// 4: scoped run state requests and replies (CM-11/12).
const Proto = 4

// MaxParallelBatch bounds a batch independently of its execution width.
const MaxParallelBatch = 4

// Tool-channel frame types. A frame whose type is none of these — including
// the empty string an old child's bare Response unmarshals to — is a protocol
// error, never a silently ignored message.
const (
	// FrameCall is the CHILD asking the parent to run one of the node's tools.
	FrameCall = "call"
	// FrameBatch carries a complete ordered group of calls.
	FrameBatch = "batch"
	// FrameResult is the PARENT answering one call.
	FrameResult = "result"
	// FrameBudget is the parent telling the child its call budget is spent, so
	// the child refuses the next call without a round trip.
	FrameBudget = "budget"
)

// Frame is one message on the tool channel, newline-delimited JSON in both
// directions.
//
// One struct for both directions rather than two: the fields a direction does
// not use are empty, and the type is what decides. Two structs would let a
// reader accept a frame the other side never sends, which is the shape a
// protocol confusion takes.
type Frame struct {
	Type string `json:"type"`
	// ID correlates a result with its call. The child mints it, strictly
	// increasing from 1; the parent echoes it. Batch results are correlated
	// against exactly the IDs sent in that batch.
	ID int64 `json:"id"`

	// Name and Input are set on FrameCall.
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Calls []Frame         `json:"calls,omitempty"`

	// Text and IsError are set on FrameResult. They are the tool_result the
	// node's own loop would have received: the text the model sees, and
	// whether the tool reported a failure. A refused call is a RESULT with
	// IsError, never an error on the channel — refusing is what the gate does
	// on the direct path too, and a script must be able to catch it.
	Text    string `json:"text,omitempty"`
	IsError bool   `json:"is_error,omitempty"`

	// Truncated says the parent cut Text at the result bound. The script sees
	// the cut rather than a clean string, because a script that aggregates a
	// cut result and returns a confident total is worse than one that knows.
	Truncated bool `json:"truncated,omitempty"`

	StateCode string `json:"state_code,omitempty"`

	// Remaining is the call budget left after this result, set on FrameResult
	// and FrameBudget. The child uses it to refuse locally; the parent's own
	// count is what binds.
	Remaining int64 `json:"remaining"`
}

// ToolOutcome is what serving one call produced: the tool_result text the
// model would have seen, and whether the tool reported a failure.
type ToolOutcome struct {
	Text    string
	IsError bool
}

// ToolServer answers a script's tool call.
//
// This is the seam, and it is declared HERE rather than in the package that
// implements it so the dependency points the legal way: pkg/backend/model
// imports pkg/scripthost, never the reverse. The implementation is the node's
// own tool dispatcher, which is why this interface says nothing about tools,
// permissions or hooks — everything that makes a nested call indistinguishable
// from a direct one lives behind CallTool, in the package that owns the door.
//
// CallTool returns an error ONLY to abort the whole script: the one case today
// is a tool whose verdict needs the operator, which a script cannot wait for.
// Every ordinary failure — an unknown tool, a malformed input, a refused
// permission, a tool that errored — is a ToolOutcome with IsError, because
// that is what the direct path produces and a script must be able to branch on
// it.
type ToolServer interface {
	CallTool(ctx context.Context, name string, input json.RawMessage) (ToolOutcome, error)
}

// errScriptAborted is what the pump returns when the server aborted the
// script. It is not a refusal: the caller of [Host.Run] gets the server's own
// error, because the thing that must happen next (pause the run, ask the
// operator) is the caller's business and not the script's.
var errScriptAborted = errors.New("scripthost: the tool server aborted the script")

// serveToolCalls validates and reserves a whole batch before executing it.
// The parent owns admission and reply writes, even when workers overlap.
func serveToolCalls(ctx context.Context, calls io.Reader, replies io.Writer, srv ToolServer, limits Limits) (int64, error) {
	br := bufio.NewReaderSize(calls, 64<<10)
	enc := json.NewEncoder(replies)
	var served, wantID int64
	for ctx.Err() == nil {
		line, err := readFrameLine(br, callFrameCap())
		if ctx.Err() != nil || errors.Is(err, io.EOF) {
			return served, nil
		}
		if err != nil {
			return served, fmt.Errorf("scripthost: reading the tool channel: %w", err)
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return served, fmt.Errorf("scripthost: decoding a tool-channel frame: %w", err)
		}
		if f.Type == frameStateStore || f.Type == frameStateLoad {
			wantID++
			if f.ID != wantID || len(f.Calls) != 0 || len(line) > stateFrameCap {
				return served, fmt.Errorf("scripthost: invalid state frame sequence")
			}
			reply := budgetFrame(f.ID, limits.ToolCalls)
			if served < limits.ToolCalls {
				served++
				reply, err = serveState(ctx, srv, f, limits.ToolCalls-served)
				if ctx.Err() != nil {
					return served, nil
				}
				if err != nil {
					return served, err
				}
			}
			if err := writeStateFrame(replies, reply); err != nil {
				if ctx.Err() != nil {
					return served, nil
				}
				return served, fmt.Errorf("scripthost: answering state request: %w", err)
			}
			continue
		}

		if f.Type != FrameCall && f.Type != FrameBatch {
			return served, fmt.Errorf("scripthost: tool channel only carries %q or %q from the child, got %q", FrameCall, FrameBatch, f.Type)
		}
		batch := []Frame{f}
		if f.Type == FrameBatch {
			batch = f.Calls
			if len(batch) == 0 || len(batch) > MaxParallelBatch {
				return served, fmt.Errorf("scripthost: invalid tool batch size %d", len(batch))
			}
		}
		for _, item := range batch {
			if item.Type != FrameCall || len(item.Calls) != 0 {
				return served, fmt.Errorf("scripthost: unexpected tool-channel frame %q", item.Type)
			}
			wantID++
			if item.ID != wantID {
				return served, fmt.Errorf("scripthost: tool call id %d arrived where %d was expected: the channel is out of step", item.ID, wantID)
			}
			if len(item.Input) > MaxToolInputBytes {
				return served, fmt.Errorf("scripthost: input for call %d exceeds %d bytes", item.ID, MaxToolInputBytes)
			}
		}
		admitted := min(int64(len(batch)), max(0, limits.ToolCalls-served))
		served += admitted
		remaining := limits.ToolCalls - served
		outcomes, err := dispatchBatch(ctx, srv, batch[:admitted], limits, remaining)
		if err != nil {
			return served, err
		}
		if ctx.Err() != nil {
			return served, nil
		}
		for _, item := range batch[admitted:] {
			outcomes = append(outcomes, budgetFrame(item.ID, limits.ToolCalls))
		}
		for _, reply := range outcomes {
			if err := enc.Encode(reply); err != nil {
				if ctx.Err() != nil {
					return served, nil
				}
				return served, fmt.Errorf("scripthost: answering tool call %d: %w", reply.ID, err)
			}
		}
	}
	return served, nil
}

func budgetFrame(id, limit int64) Frame {
	return Frame{Type: FrameBudget, ID: id, IsError: true, Text: fmt.Sprintf("the script has spent its budget of %d tool calls. Aggregate in the script, or ask the operator to raise the bound.", limit)}
}

// dispatchBatch executes exclusive tools between waves of explicitly safe
// tools. Each wave joins before the next starts; workers own distinct slots.
func dispatchBatch(ctx context.Context, srv ToolServer, calls []Frame, limits Limits, remaining int64) ([]Frame, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	replies := make([]Frame, len(calls))
	errs := make([]error, len(calls))
	run := func(i int) {
		if ctx.Err() != nil {
			return
		}
		f := calls[i]
		out, err := srv.CallTool(ctx, f.Name, f.Input)
		if err != nil {
			if ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
				errs[i] = err
			}
			cancel()
			return
		}
		text, cut := clipToolText(out.Text, limits.ToolResultBytes)
		replies[i] = Frame{Type: FrameResult, ID: f.ID, Text: text, IsError: out.IsError, Truncated: cut, Remaining: remaining}
	}
	width := int(max(1, min(limits.ConcurrentToolCalls, MaxParallelBatch)))
	eligibility, _ := srv.(interface{ ConcurrentSafe(string) bool })
	safe := func(i int) bool { return width > 1 && eligibility != nil && eligibility.ConcurrentSafe(calls[i].Name) }
	for i := 0; i < len(calls) && ctx.Err() == nil; {
		j := i + 1
		if safe(i) {
			for j < len(calls) && j-i < width && safe(j) {
				j++
			}
		}
		if j == i+1 {
			run(i)
		} else {
			var workers sync.WaitGroup
			for k := i; k < j; k++ {
				workers.Go(func() { run(k) })
			}
			workers.Wait()
		}
		if abort := errors.Join(errs[i:j]...); abort != nil {
			return nil, fmt.Errorf("%w: %w", errScriptAborted, abort)
		}
		i = j
	}
	return replies, nil
}

// readFrameLine reads one newline-delimited frame, refusing a line longer than
// the bound instead of growing a buffer the child chose the size of.
//
// bufio.Reader.ReadBytes would grow without limit, which hands the child a
// memory bound on the PARENT — the one process the child's rlimits do not
// cover.
func readFrameLine(br *bufio.Reader, max int64) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if int64(len(out)) > max {
			return nil, fmt.Errorf(
				"a tool-channel frame passed %d bytes: the child is framing something this channel does not carry", max)
		}
		if !isPrefix {
			if len(out) == 0 {
				// A blank line is not a frame. Skip it rather than failing:
				// json.Encoder writes exactly one newline per value, so a
				// blank line means something else wrote to the pipe.
				continue
			}
			return out, nil
		}
	}
}

// clipToolText cuts a tool result at the bound and says so.
//
// The cut is on BYTES and lands on a rune boundary, so a script that counts
// characters is not handed half a code point. A tool result is text the model
// would have read, not a value the script parses, so cutting it is honest in a
// way cutting a JSON value would not be.
func clipToolText(s string, max int64) (string, bool) {
	// No max<=0 escape hatch, for the same reason resultFrameCap dropped
	// its zero branch: a clip bound of zero CLIPS TO EMPTY (bounded, and
	// flagged) rather than returning the caller's whole string unbounded.
	if int64(len(s)) <= max {
		return s, false
	}
	cut := int(max)
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// utf8Start reports whether b can begin a UTF-8 sequence — i.e. it is not a
// continuation byte.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// callFrameCap bounds one CALL frame — the child-to-parent direction, whose
// payload is an input the script composed and whose only bound is the child's
// own [MaxToolInputBytes].
//
// Derived from that bound rather than from [Limits.ScriptBytes], which is
// what stood here. Two caps bounded the same frame and were never compared:
// the child admits an input up to MaxToolInputBytes (256 KiB) and the parent
// refused the frame carrying it past ScriptBytes, whose DEFAULT is also
// 256 KiB — so the ~58-byte envelope was the entire window and a 262100-byte
// input, legal to the child, was refused by the parent with NO declaration
// involved. The author surface made it worse by letting `script_bytes` be
// lowered, which moved the window to everything above the declaration.
//
// A cap is still kept, for the reason TestAnOversizedFrameIsRefusedRatherThanBuffered
// states: the parent is the one process no rlimit covers, so it must not
// buffer whatever a compromised child chooses to write.
func callFrameCap() int64 {
	// The envelope: type, id, name and the remaining count. Small and fixed.
	const envelope = 1 << 10
	return MaxToolInputBytes + envelope
}

// resultFrameCap bounds one RESULT frame — the parent-to-child direction,
// whose payload the parent itself clipped to [Limits.ToolResultBytes].
//
// It is a derived number rather than a Limits field on purpose: a frame size
// is a property of this protocol, not a bound an operator chooses, and an
// eighth Limits field would owe a DSL surface it should not have.
//
// The factor is JSON escaping, not a margin: `json.Encode` renders a control
// byte as `\uXXXX`, six bytes for one, and the parent clips on a rune
// boundary so the text it sends is valid UTF-8 but may be entirely control
// characters. A cap below that refuses a payload the parent was told to send,
// which in a blocking path costs as much as a hole — the frame error is not
// even a refusal the model can act on, it is classified as a broken
// deployment. The envelope (type, id, flags, the remaining count) is small
// and fixed; 1 KiB covers it.
//
// The CALL direction has [callFrameCap] above, for the same reason and since
// the same commit: an operator's `script_bytes` described neither frame. The
// draft of this comment that said the call direction "keeps
// Limits.ScriptBytes" was true for twenty minutes and false by the time it
// shipped — the commit that wrote it moved that site too.
func resultFrameCap(l Limits) int64 {
	const jsonEscapeWorstCase = 6
	const envelope = 1 << 10
	// No zero fallback, on purpose: limits arrive CLAMPED — Host.Run and
	// RunChild clamp before use — so a caller that skips the clamp gets a
	// loud refusal at the first oversized frame, never a silent default it
	// did not choose. The ticket this closes called the previous
	// zero-branch the inconsistent half of a defence: it silently
	// substituted the default while clipToolText returned unbounded.
	return l.ToolResultBytes*jsonEscapeWorstCase + envelope
}
