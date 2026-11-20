package scripthost

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/dop251/goja"

	"github.com/SocialGouv/iterion/pkg/scripthost/toolindex"
)

// Tool-channel descriptor numbers in the CHILD. The parent hands them over at
// spawn through cmd.ExtraFiles, which numbers them from 3 in order.
//
// They are ALREADY OPEN when [Confine] runs, which is the whole reason this
// works: a deny-all Landlock ruleset does not revoke a descriptor, it stops
// opening one. So the channel survives the confinement that makes the child
// unable to open anything else — and nothing in the binding ever opens a path,
// a socket or a process.
//
// The precedent for the numbering is pkg/backend/mcp's host runner, which
// hands Chromium the same pair in the same order.
const (
	fdToolReplies = 3 // the child READS the parent's results here
	fdToolCalls   = 4 // the child WRITES its calls here
)

// MaxToolInputBytes caps the JSON a script may hand one tool.
//
// It is not negotiable per-run and so is not a [Limits] field: it bounds a
// FRAME, and the frame reader on the other side bounds the same thing. A
// script that needs to pass more than this to a tool is not composing calls,
// it is moving a payload, which is what a file is for.
const MaxToolInputBytes = 256 << 10

// toolBridge is the child's half of the tool channel.
//
// One call in flight, because that is the v0 contract and because the VM is
// single-threaded: the host function below BLOCKS the interpreter until the
// parent answers, so there is nothing to interleave. That also means no
// promise: a bare goja.Runtime pumps no job queue, so a promise this package
// returned would never settle and the script would read a pending object as
// its result. A synchronous round trip is the only shape this runtime can
// honestly offer.
type toolBridge struct {
	in     *bufio.Reader
	out    io.Writer
	enc    *json.Encoder
	limits Limits

	nextID    int64
	remaining int64
	// spent latches the budget: once the parent has said the budget is gone,
	// the bridge refuses locally and the channel sees nothing more. Without
	// the latch a loop would keep paying a round trip per refusal.
	spent bool

	// index answers `call.describe` and `call.search` (CM-3). It is built in
	// the child from the request's catalogue, so a lookup costs no round trip
	// and the parent serves nothing but calls. Nil means an empty index.
	index *toolindex.Index
}

func newToolBridge(replies io.Reader, calls io.Writer, limits Limits) *toolBridge {
	return &toolBridge{
		in:        bufio.NewReaderSize(replies, 64<<10),
		out:       calls,
		enc:       json.NewEncoder(calls),
		limits:    limits,
		remaining: limits.ToolCalls,
	}
}

// errToolChannelBroken is a PROTOCOL failure: the parent and the child
// disagree about the wire, or the channel died under a call. It is not a
// script fault and must not be reported as one — [runScript] turns it into a
// host-died verdict rather than a script_error, because a script that is told
// "your call failed" would retry a broken deployment in a loop.
var errToolChannelBroken = errors.New("scripthost: the tool channel is broken")

// install binds the one host function a script gets.
//
// ONE function, named for what it does to the node's own tools. Everything a
// later slice adds — concurrency, structured results, store/load — travels as
// arguments or as new frame types, not as a second global: the script surface
// is part of the prompt cache's prefix, so churning it churns the cache.
func (b *toolBridge) install(vm *goja.Runtime) error {
	if err := vm.Set("call", b.call(vm)); err != nil {
		return err
	}
	return b.installDiscovery(vm)
}

// installDiscovery hangs `describe` and `search` on `call` itself (CM-3).
//
// Methods of the one global rather than globals of their own, for the reason
// install states: the script surface is part of the prompt cache's prefix.
// Both answer from the index the child built from the request's catalogue —
// the tools the parent's door serves — so a program cannot discover a tool
// the door would refuse. Their values are strings and fresh objects of
// strings: no Go value is reflected into the VM.
func (b *toolBridge) installDiscovery(vm *goja.Runtime) error {
	index := b.index
	if index == nil {
		index = toolindex.New(nil, 0)
	}
	call := vm.Get("call").ToObject(vm)
	if err := call.Set("describe", func(fc goja.FunctionCall) goja.Value {
		arg := fc.Argument(0)
		if goja.IsUndefined(arg) || goja.IsNull(arg) || arg.String() == "" {
			panic(vm.NewTypeError("call.describe(name): the tool name is required"))
		}
		name := arg.String()
		decl, ok := index.Describe(name)
		if !ok {
			panic(vm.NewTypeError("call.describe(%q): no tool this program can call is named that — call.search finds one by what it does", name))
		}
		return vm.ToValue(decl)
	}); err != nil {
		return err
	}
	return call.Set("search", func(fc goja.FunctionCall) goja.Value {
		arg := fc.Argument(0)
		if goja.IsUndefined(arg) || goja.IsNull(arg) {
			panic(vm.NewTypeError("call.search(query, limit): the query is required"))
		}
		limit := 0
		if l := fc.Argument(1); !goja.IsUndefined(l) && !goja.IsNull(l) {
			limit = int(l.ToInteger())
		}
		hits := index.Search(arg.String(), limit)
		out := make([]any, 0, len(hits))
		for _, h := range hits {
			obj := vm.NewObject()
			must(obj.Set("name", h.Name))
			must(obj.Set("summary", h.Summary))
			must(obj.Set("declaration", h.Declaration))
			out = append(out, obj)
		}
		return vm.NewArray(out...)
	})
}

// call is the binding behind `call(name, input)`.
func (b *toolBridge) call(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(fc goja.FunctionCall) goja.Value {
		name := fc.Argument(0).String()
		if name == "" || goja.IsUndefined(fc.Argument(0)) {
			panic(vm.NewTypeError("call(name, input): the tool name is required"))
		}

		// The input is encoded by the ENGINE's JSON.stringify, for the same
		// reason the result value is: Export() + json.Marshal panics in Go on a
		// throwing getter or a Proxy trap, loses key order, and flattens an
		// Error or a RegExp to {}. Here the stakes are higher than cosmetic —
		// the encoded bytes are what the node's tool actually receives.
		input := []byte("{}")
		if arg := fc.Argument(1); !goja.IsUndefined(arg) && !goja.IsNull(arg) {
			encoded, err := encodeValue(vm, arg)
			if err != nil {
				panic(vm.NewTypeError("call(%q, input): the input cannot be encoded as JSON: %s", name, safeErrorText(err)))
			}
			input = []byte(encoded)
		}
		if int64(len(input)) > MaxToolInputBytes {
			panic(vm.NewTypeError("call(%q, input): the input is %d bytes, over the %d-byte bound",
				name, len(input), MaxToolInputBytes))
		}
		// And the FRAME, measured the way the parent measures it. Bounding
		// only the input left the two caps on one frame still uncompared:
		// the NAME is `fc.Argument(0).String()` — the script picks it, with
		// no bound of its own — so a legal 262143-byte input with a 1 KiB
		// name was measured producing a frame the parent refuses, and a
		// refused call frame costs the whole wall deadline before the model
		// is told the deployment is broken (filed as #3316). An earlier
		// comment called the envelope "small and fixed"; a script-chosen
		// name is neither.
		//
		// Refused HERE it is a catchable TypeError naming the number, which
		// is a thing a program can act on, and it is the same side that owns
		// the name.
		if frame, err := json.Marshal(&Frame{Type: FrameCall, ID: b.nextID + 1, Name: name, Input: input}); err == nil {
			if int64(len(frame)) > callFrameCap() {
				panic(vm.NewTypeError("call(%q, input): the call is %d bytes framed, over the %d-byte channel bound — the tool NAME counts too",
					name, len(frame), callFrameCap()))
			}
		}

		out, err := b.roundTrip(name, input)
		if err != nil {
			// A broken channel is not the script's fault and must not look
			// like it. Interrupting is how a non-script failure leaves a
			// script: goja's interrupt cannot be caught by `catch` or
			// `finally`, so no script can swallow it and keep going.
			vm.Interrupt(err)
			panic(vm.NewGoError(err))
		}

		if out.IsError {
			// A failed call THROWS, so a script that ignores the outcome stops
			// instead of aggregating an error message as if it were data. The
			// throw is CATCHABLE — a refused call must let a script return what
			// it has — and the thrown value carries the text, so
			// `try { call(...) } catch (e) { e.text }` recovers it.
			//
			// An Error INSTANCE, with the fields set on it: `e instanceof
			// Error` holds and goja attaches its stack. The first version
			// wrapped a Go error carrying the object, which threw goja's own
			// Error instead and handed the script `e.text === undefined` — the
			// text was on a value nobody could reach.
			thrown := vm.NewGoError(fmt.Errorf("tool %q failed: %s", name, out.Text))
			must(thrown.Set("tool", name))
			must(thrown.Set("text", out.Text))
			must(thrown.Set("isError", true))
			must(thrown.Set("truncated", out.Truncated))
			must(thrown.Set("remaining", out.Remaining))
			// goja's exceptionFromValue takes a *Object as the thrown VALUE,
			// which is what makes the fields above reachable from `catch`.
			panic(thrown)
		}

		obj := vm.NewObject()
		must(obj.Set("text", out.Text))
		must(obj.Set("isError", false))
		must(obj.Set("truncated", out.Truncated))
		must(obj.Set("remaining", out.Remaining))
		return obj
	}
}

// roundTrip writes one call and blocks for its result.
func (b *toolBridge) roundTrip(name string, input []byte) (Frame, error) {
	if b.spent {
		// Refused locally, with the SAME shape the parent's refusal has: the
		// script must not be able to tell the latch from the round trip, or a
		// loop would behave differently on its first refusal than on its
		// hundredth. An empty frame here would have read as a successful call
		// returning nothing.
		return Frame{
			Type:    FrameBudget,
			ID:      b.nextID,
			IsError: true,
			Text: fmt.Sprintf("the script has spent its budget of %d tool calls. Aggregate in the script, "+
				"or ask the operator to raise the bound.", b.limits.ToolCalls),
		}, nil
	}
	b.nextID++
	if err := b.enc.Encode(&Frame{Type: FrameCall, ID: b.nextID, Name: name, Input: input}); err != nil {
		return Frame{}, fmt.Errorf("%w: writing call %d: %v", errToolChannelBroken, b.nextID, err)
	}

	// The cap is derived from what this frame CARRIES, not from the source
	// size. A result frame holds text the parent already clipped to
	// ToolResultBytes, so resultFrameCap is the bound that describes it;
	// ScriptBytes stood here and describes a program's length, which is a
	// different number that happened to be larger while neither could be
	// declared. Measured once both could: `tool_result_bytes: 524288` —
	// half its own platform ceiling — with the DEFAULT 256 KiB
	// `script_bytes` made the child refuse a frame its own parent had
	// written, as "a tool-channel frame passed 262144 bytes".
	//
	// An earlier draft of this comment added "and codemodeTool classifies
	// that as a deployment failure". Measured: it does not. That branch
	// fires only when Host.Run returns an error, which is the CALL
	// direction. This refusal comes back as a `!resp.OK()` verdict, and
	// WHICH one depends on whether the program has a try/catch —
	// script_error without it, script_host_died with. That flip is a defect
	// of its own, pre-existing and filed: errToolChannelBroken's own doc
	// says the host-died class is what a broken channel must produce,
	// precisely so a model is not told "your call failed" about a channel.
	line, err := readFrameLine(b.in, resultFrameCap(b.limits))
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, fmt.Errorf("%w: the parent closed the channel while call %d was in flight",
				errToolChannelBroken, b.nextID)
		}
		return Frame{}, fmt.Errorf("%w: reading the result of call %d: %v", errToolChannelBroken, b.nextID, err)
	}

	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, fmt.Errorf("%w: the parent's answer to call %d is not a frame: %v",
			errToolChannelBroken, b.nextID, err)
	}
	if f.ID != b.nextID {
		return Frame{}, fmt.Errorf("%w: the answer to call %d carries id %d",
			errToolChannelBroken, b.nextID, f.ID)
	}
	switch f.Type {
	case FrameResult:
		b.remaining = f.Remaining
		return f, nil
	case FrameBudget:
		b.spent = true
		b.remaining = 0
		return f, nil
	default:
		return Frame{}, fmt.Errorf("%w: the parent answered call %d with a %q frame",
			errToolChannelBroken, b.nextID, f.Type)
	}
}

// must panics on a Set that cannot fail in practice, rather than discarding
// its error: a silently half-built result object is a wrong answer, and this
// package's rule is that an error says what failed.
func must(err error) {
	if err != nil {
		panic(fmt.Errorf("scripthost: building a tool result object: %w", err))
	}
}
