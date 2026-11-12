package scripthost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
const Proto = 1

// Tool-channel frame types. A frame whose type is none of these — including
// the empty string an old child's bare Response unmarshals to — is a protocol
// error, never a silently ignored message.
const (
	// FrameCall is the CHILD asking the parent to run one of the node's tools.
	FrameCall = "call"
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
	// increasing from 1; the parent echoes it. v0 keeps ONE call in flight, so
	// the id is a check rather than a demultiplexer — a result whose id is not
	// the one awaited is a protocol error, not a queued message.
	ID int64 `json:"id"`

	// Name and Input are set on FrameCall.
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

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

// serveToolCalls is the parent's half of the tool channel: it reads calls the
// child writes, asks the server, and writes results back, one at a time.
//
// ONE call in flight is the v0 contract, and it is not a simplification to be
// apologised for: the node's dispatcher re-derives its executable-tool pin on
// every dispatch and the existing direct loop is serial too, so a concurrent
// script would be the first caller to ask whether that pin is safe to share.
// Concurrency is its own slice, and the frame carries an id so it does not
// need a new wire.
//
// The PARENT counts the calls. The child counts too and refuses locally to
// save a round trip, but a child is the thing being bounded and cannot be the
// thing that bounds: a child that lied about its count would get the budget it
// claimed. The count here is what binds.
//
// It returns when the child closes the channel (the ordinary end), when the
// context ends, or on the first protocol error. A protocol error is a HOST
// error: the two sides disagree about the wire, which is a deployment fault
// and not something a script did.
func serveToolCalls(ctx context.Context, calls io.Reader, replies io.Writer, srv ToolServer, limits Limits) (int64, error) {
	br := bufio.NewReaderSize(calls, 64<<10)
	enc := json.NewEncoder(replies)
	var served, wantID int64

	for {
		// A context that ended is NOT a channel failure and must not be
		// reported as one: the wall bound firing is the ordinary way a script
		// dies, and the verdict for it is decided from the child's death, not
		// from here. Returning ctx.Err() made every wall-clock death look like
		// a broken deployment.
		if ctx.Err() != nil {
			return served, nil
		}
		line, err := readFrameLine(br, limits.ScriptBytes)
		switch {
		case errors.Is(err, io.EOF):
			// The child closed its end: the script finished, or it was killed.
			// Either way the verdict is on stdout, not here.
			return served, nil
		case err != nil:
			return served, fmt.Errorf("scripthost: reading the tool channel: %w", err)
		}

		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return served, fmt.Errorf("scripthost: decoding a tool-channel frame: %w", err)
		}
		if f.Type != FrameCall {
			return served, fmt.Errorf(
				"scripthost: the child sent a %q frame on the tool channel, which only carries %q",
				f.Type, FrameCall)
		}
		wantID++
		if f.ID != wantID {
			return served, fmt.Errorf(
				"scripthost: tool call id %d arrived where %d was expected: the channel is out of step",
				f.ID, wantID)
		}

		// The budget is checked BEFORE the dispatch, so an over-budget call
		// costs nothing: no hook fires, no permission verdict is computed, no
		// event is written.
		if served >= limits.ToolCalls {
			reply := Frame{Type: FrameBudget, ID: f.ID, Remaining: 0,
				IsError: true,
				Text: fmt.Sprintf(
					"the script has spent its budget of %d tool calls. Aggregate in the script, "+
						"or ask the operator to raise the bound.", limits.ToolCalls)}
			if err := enc.Encode(&reply); err != nil {
				return served, fmt.Errorf("scripthost: answering an over-budget call: %w", err)
			}
			continue
		}

		out, abort := srv.CallTool(ctx, f.Name, f.Input)
		if abort != nil {
			return served, fmt.Errorf("%w: %w", errScriptAborted, abort)
		}
		served++

		text, cut := clipToolText(out.Text, limits.ToolResultBytes)
		reply := Frame{
			Type:      FrameResult,
			ID:        f.ID,
			Text:      text,
			IsError:   out.IsError,
			Truncated: cut,
			Remaining: limits.ToolCalls - served,
		}
		if err := enc.Encode(&reply); err != nil {
			return served, fmt.Errorf("scripthost: answering tool call %d: %w", f.ID, err)
		}
	}
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
	if max <= 0 || int64(len(s)) <= max {
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
