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
// 5: structured results on result frames (CM-7). A version-4 child decodes a
// result carrying them fine — unknown JSON fields are ignored — but its
// programs would read a tool's structured payload as undefined, a silent
// wrong answer of exactly the kind the refusal exists to prevent.
// 6: the child's confinement as a FIRST stdout document, written once it is
// confined and before the script runs, so a child killed mid-script — a
// deployment's stricter CPU bound, a cgroup kill — has still said what it
// enforced (#3451). Not a tool-channel frame: the child writes it to a
// parent whose Request carries Proto ≥ 6, whatever the channel, because a
// version-5 parent decodes stdout's FIRST document as the verdict and would
// read a confinement-only document as a host that died.
const Proto = 6

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

	// Structured is the tool's structured result (CM-7), compact JSON the
	// tool produced alongside its text, handed to the program as a value. It
	// is set on a FrameResult only: a failed call throws and carries no
	// structured payload, and a refused call never had one. An absent payload
	// means one of three things — the tool sent none, the tool failed, or the
	// payload did not fit the bound — and the next two fields say which.
	Structured json.RawMessage `json:"structured,omitempty"`
	// StructuredTruncated says the parent dropped Structured at the result
	// bound. Explicit only when something was actually dropped: a payload
	// that fits leaves the field unset, so a program reading it false knows
	// the absence of Structured means the tool sent none.
	StructuredTruncated bool `json:"structured_truncated,omitempty"`
	// StructuredBytes is the size of the structured payload the parent
	// dropped, set only alongside StructuredTruncated. A program that needs
	// the whole payload can say by how much it was shorted rather than
	// guessing from an empty value.
	StructuredBytes int64 `json:"structured_bytes,omitempty"`

	StateCode string `json:"state_code,omitempty"`

	// Remaining is the call budget left after this result, set on FrameResult
	// and FrameBudget. The child uses it to refuse locally; the parent's own
	// count is what binds.
	Remaining int64 `json:"remaining"`
}

// ToolOutcome is what serving one call produced: the tool_result text the
// model would have seen, and whether the tool reported a failure.
//
// Structured carries the same result's structured payload, compact JSON, as
// the tool reported it alongside its text — a server that has none leaves it
// nil, and the parent clips what it delivered past the bound. The truncation
// fields let a server report a payload IT already shortened; a server that
// delivers whole payloads leaves them zero and the parent's clip fills them.
type ToolOutcome struct {
	Text    string
	IsError bool

	Structured          []byte
	StructuredTruncated bool
	StructuredBytes     int64
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
//
// The stats it returns are the parent's own count, which is what binds: the
// child counts too, but a child is what is being bounded.
func serveToolCalls(ctx context.Context, calls io.Reader, replies io.Writer, srv ToolServer, limits Limits) (CallStats, error) {
	br := bufio.NewReaderSize(calls, 64<<10)
	enc := json.NewEncoder(replies)
	var stats CallStats
	var wantID int64
	for ctx.Err() == nil {
		line, err := readFrameLine(br, callFrameCap())
		if ctx.Err() != nil || errors.Is(err, io.EOF) {
			return stats, nil
		}
		if err != nil {
			return stats, fmt.Errorf("scripthost: reading the tool channel: %w", err)
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return stats, fmt.Errorf("scripthost: decoding a tool-channel frame: %w", err)
		}
		if f.Type == frameStateStore || f.Type == frameStateLoad {
			wantID++
			if f.ID != wantID || len(f.Calls) != 0 || len(line) > stateFrameCap {
				return stats, fmt.Errorf("scripthost: invalid state frame sequence")
			}
			reply := budgetFrame(f.ID, limits.ToolCalls)
			if stats.Served < limits.ToolCalls {
				stats.Served++
				stats.StateCalls++
				reply, err = serveState(ctx, srv, f, limits.ToolCalls-stats.Served)
				if ctx.Err() != nil {
					return stats, nil
				}
				if err != nil {
					return stats, err
				}
			} else {
				stats.BudgetRefused++
			}
			if err := writeStateFrame(replies, reply); err != nil {
				if ctx.Err() != nil {
					return stats, nil
				}
				return stats, fmt.Errorf("scripthost: answering state request: %w", err)
			}
			continue
		}

		if f.Type != FrameCall && f.Type != FrameBatch {
			return stats, fmt.Errorf("scripthost: tool channel only carries %q or %q from the child, got %q", FrameCall, FrameBatch, f.Type)
		}
		batch := []Frame{f}
		if f.Type == FrameBatch {
			batch = f.Calls
			if len(batch) == 0 || len(batch) > MaxParallelBatch {
				return stats, fmt.Errorf("scripthost: invalid tool batch size %d", len(batch))
			}
		}
		for _, item := range batch {
			if item.Type != FrameCall || len(item.Calls) != 0 {
				return stats, fmt.Errorf("scripthost: unexpected tool-channel frame %q", item.Type)
			}
			wantID++
			if item.ID != wantID {
				return stats, fmt.Errorf("scripthost: tool call id %d arrived where %d was expected: the channel is out of step", item.ID, wantID)
			}
			if len(item.Input) > MaxToolInputBytes {
				return stats, fmt.Errorf("scripthost: input for call %d exceeds %d bytes", item.ID, MaxToolInputBytes)
			}
		}
		group := callGroup{batch: f.Type == FrameBatch, first: batch[0].ID, size: len(batch)}
		admitted := min(int64(len(batch)), max(0, limits.ToolCalls-stats.Served))
		stats.Served += admitted
		remaining := limits.ToolCalls - stats.Served
		outcomes, err := dispatchBatch(ctx, srv, batch[:admitted], group, limits, remaining)
		if err != nil {
			return stats, err
		}
		if ctx.Err() != nil {
			return stats, nil
		}
		observer, _ := srv.(BudgetRefusalObserver)
		for i, item := range batch[admitted:] {
			reply := budgetFrame(item.ID, limits.ToolCalls)
			stats.BudgetRefused++
			if observer != nil {
				observer.RefusedOnBudget(group.frame(int(admitted)+i, item.ID), item.Name, item.Input, reply.Text)
			}
			outcomes = append(outcomes, reply)
		}
		for _, reply := range outcomes {
			if err := enc.Encode(reply); err != nil {
				if ctx.Err() != nil {
					return stats, nil
				}
				return stats, fmt.Errorf("scripthost: answering tool call %d: %w", reply.ID, err)
			}
		}
	}
	return stats, nil
}

func budgetFrame(id, limit int64) Frame {
	return Frame{Type: FrameBudget, ID: id, IsError: true, Text: fmt.Sprintf("the script has spent its budget of %d tool calls. Aggregate in the script, or ask the operator to raise the bound.", limit)}
}

// dispatchBatch executes exclusive tools between waves of explicitly safe
// tools. Each wave joins before the next starts; workers own distinct slots.
// Each call's server receives its [CallFrame] on its own worker context, so
// the program order a reader needs travels with the call rather than being
// rebuilt from the order workers finish in.
func dispatchBatch(ctx context.Context, srv ToolServer, calls []Frame, group callGroup, limits Limits, remaining int64) ([]Frame, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	replies := make([]Frame, len(calls))
	errs := make([]error, len(calls))
	run := func(i int) {
		if ctx.Err() != nil {
			return
		}
		f := calls[i]
		out, err := srv.CallTool(WithCallFrame(ctx, group.frame(i, f.ID)), f.Name, f.Input)
		if err != nil {
			if ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
				errs[i] = err
			}
			cancel()
			return
		}
		text, cut := clipToolText(out.Text, limits.ToolResultBytes)
		frame := Frame{Type: FrameResult, ID: f.ID, Text: text, IsError: out.IsError, Truncated: cut, Remaining: remaining}
		// A failed call throws on the child side and its thrown value carries
		// no structured fields, so a payload here would be wire bytes no
		// program could ever read. Dropped rather than sent.
		if !out.IsError {
			structured, dropped, original := clipToolStructured(out, limits.ToolResultBytes)
			frame.Structured = structured
			frame.StructuredTruncated = dropped
			frame.StructuredBytes = original
		}
		replies[i] = frame
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

// clipToolStructured cuts a tool result's structured payload at the bound,
// the sibling of [clipToolText]. The two differ in what a cut means: text is
// prose the model would have read, so it is cut on a rune boundary and the
// cut is honest to read; structured JSON is cut ONLY ALL-OR-NOTHING — half a
// JSON document is not a value a program can parse, so past the bound the
// payload goes entirely, flagged, with the size it had.
//
// The payload arrives pre-encoded by the server (a sink of what the tool
// reported), so the control characters inside it are already escaped in the
// payload itself. It still shares text's wire worst case: the frame encoder
// re-escapes <, > and & wherever it meets them, inside a RawMessage too, so
// one payload byte can reach the wire as six — and [resultFrameCap] spends
// the same factor on it.
func clipToolStructured(out ToolOutcome, max int64) (structured []byte, dropped bool, original int64) {
	if int64(len(out.Structured)) > max {
		return nil, true, int64(len(out.Structured))
	}
	return out.Structured, out.StructuredTruncated, out.StructuredBytes
}

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
// Since CM-7 the frame may also carry the tool's structured payload, up to
// ToolResultBytes of it ([clipToolStructured]). That payload is pre-encoded
// JSON, but pre-encoded is not verbatim on the wire: the frame encoder
// re-escapes <, > and & inside a RawMessage exactly as it does in the text,
// six bytes for one, so the payload's own worst case is the same factor. A
// first draft spent 1 on the structured half — "pre-encoded travels
// verbatim" — and a probe of json.Encoder refuted it before it shipped; the
// cap spends 6 on each, which is what a frame holding a full bound of both
// actually measures.
//
// The CALL direction has [callFrameCap] above, for the same reason and since
// the same commit: an operator's `script_bytes` described neither frame. The
// draft of this comment that said the call direction "keeps
// Limits.ScriptBytes" was true for twenty minutes and false by the time it
// shipped — the commit that wrote it moved that site too.
func resultFrameCap(l Limits) int64 {
	const jsonEscapeWorstCase = 6
	const structuredWorstCase = 6
	const envelope = 1 << 10
	// No zero fallback, on purpose: limits arrive CLAMPED — Host.Run and
	// RunChild clamp before use — so a caller that skips the clamp gets a
	// loud refusal at the first oversized frame, never a silent default it
	// did not choose. The ticket this closes called the previous
	// zero-branch the inconsistent half of a defence: it silently
	// substituted the default while clipToolText returned unbounded.
	return l.ToolResultBytes*(jsonEscapeWorstCase+structuredWorstCase) + envelope
}
