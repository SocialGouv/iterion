package scripthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/dop251/goja"

	"github.com/SocialGouv/iterion/pkg/scripthost/toolindex"
)

const (
	// DefaultStoreEntries and DefaultStoreBytes apply to a run without overrides.
	DefaultStoreEntries = 32
	DefaultStoreBytes   = 128 << 10
	// MaxStoreEntries and MaxStoreBytes are the platform ceilings.
	MaxStoreEntries = 64
	MaxStoreBytes   = 512 << 10
	// MaxStateKeyBytes and MaxStateValueBytes bound one state operation.
	MaxStateKeyBytes   = 128
	MaxStateValueBytes = 16 << 10
	frameStateStore    = "state_store"
	frameStateLoad     = "state_load"
	frameStateResult   = "state_result"
	stateFrameCap      = MaxStateValueBytes*6 + MaxStateKeyBytes*6 + 1024
)

var (
	ErrStateUnavailable      = errors.New("codemode run state is unavailable")
	ErrStateQuota            = errors.New("codemode run state quota exceeded")
	ErrStateInvalid          = errors.New("invalid codemode state key or value")
	ErrStateWriteUnconfirmed = errors.New("codemode state write was not acknowledged; it may have committed")
)

type stateServer interface {
	StoreState(context.Context, string, json.RawMessage) error
	LoadState(context.Context, string) (json.RawMessage, bool, error)
}

func validStateKey(key string) bool {
	return key != "" && len(key) <= MaxStateKeyBytes && utf8.ValidString(key)
}

// stateKey validates the original UTF-16 code units before Go conversion can
// replace an unpaired surrogate with U+FFFD and alias an existing key.
func stateKey(arg goja.Value) (string, bool) {
	key, ok := arg.(goja.String)
	if !ok || key.Length() == 0 || key.Length() > MaxStateKeyBytes {
		return "", false
	}
	for i := 0; i < key.Length(); i++ {
		unit := key.CharAt(i)
		switch {
		case unit >= 0xD800 && unit <= 0xDBFF:
			i++
			if i == key.Length() || key.CharAt(i) < 0xDC00 || key.CharAt(i) > 0xDFFF {
				return "", false
			}
		case unit >= 0xDC00 && unit <= 0xDFFF:
			return "", false
		}
	}
	text := key.String()
	return text, validStateKey(text)
}

func validStateValue(value json.RawMessage) bool {
	if len(value) > MaxStateValueBytes || !utf8.Valid(value) || !json.Valid(value) {
		return false
	}
	// JSON syntax allows unpaired surrogate escapes, but the VM's parser
	// replaces them with U+FFFD. Reject them in strings and property names.
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			continue
		}
		i++
		if value[i] != 'u' {
			continue
		}
		unit, _ := strconv.ParseUint(string(value[i+1:i+5]), 16, 16)
		i += 4
		switch {
		case unit >= 0xD800 && unit <= 0xDBFF:
			if len(value)-i < 7 || value[i+1] != '\\' || value[i+2] != 'u' {
				return false
			}
			low, _ := strconv.ParseUint(string(value[i+3:i+7]), 16, 16)
			if low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		case unit >= 0xDC00 && unit <= 0xDFFF:
			return false
		}
	}
	return true
}

func serveState(ctx context.Context, srv ToolServer, f Frame, remaining int64) (Frame, error) {
	reply := Frame{Type: frameStateResult, ID: f.ID, Remaining: remaining}
	state, ok := srv.(stateServer)
	var err error
	switch {
	case !validStateKey(f.Name) || (f.Type == frameStateStore && !validStateValue(f.Input)) || (f.Type == frameStateLoad && len(f.Input) != 0):
		err = ErrStateInvalid
	case !ok:
		err = ErrStateUnavailable
	case f.Type == frameStateStore:
		err = state.StoreState(ctx, f.Name, f.Input)
	default:
		var found bool
		reply.Input, found, err = state.LoadState(ctx, f.Name)
		if err == nil && found && !validStateValue(reply.Input) {
			return Frame{}, errors.New("scripthost: run state returned invalid JSON or exceeded the value bound")
		}
		if !found {
			reply.Input = nil
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrStateUnavailable):
		reply.StateCode = "state_unavailable"
	case errors.Is(err, ErrStateInvalid):
		reply.StateCode = "state_invalid"
	case errors.Is(err, ErrStateQuota):
		reply.StateCode = "state_quota"
	case errors.Is(err, ErrStateWriteUnconfirmed):
		reply.StateCode = "state_write_unconfirmed"
	default:
		return Frame{}, errors.New("scripthost: run state operation failed")
	}
	if reply.StateCode != "" {
		reply.Input = nil
	}
	return reply, nil
}

func (b *toolBridge) installState(vm *goja.Runtime) error {
	// The engine's JSON.parse was resolved by install, before any program
	// ran; a program that replaces JSON.parse cannot disarm a state value's
	// decoding either.
	parse := b.parse
	bind := func(op string) func(goja.FunctionCall) goja.Value {
		return func(fc goja.FunctionCall) goja.Value {
			key, ok := stateKey(fc.Argument(0))
			if !ok {
				panic(vm.NewTypeError("codemode state requires a nonempty key of at most %d UTF-8 bytes", MaxStateKeyBytes))
			}
			f := Frame{Type: op, Name: key}
			if op == frameStateStore {
				value, err := encodeJSONValue(vm, fc.Argument(1), true)
				f.Input = json.RawMessage(value)
				if err != nil || !validStateValue(f.Input) {
					panic(vm.NewTypeError("call.store requires valid JSON of at most %d bytes", MaxStateValueBytes))
				}
			}
			r, err := b.stateTrip(f)
			if err != nil {
				vm.Interrupt(err)
				panic(vm.NewGoError(err))
			}
			if r.StateCode != "" || r.Type == FrameBudget {
				code, message := r.StateCode, ""
				switch code {
				case "state_unavailable":
					message = ErrStateUnavailable.Error()
				case "state_invalid":
					message = ErrStateInvalid.Error()
				case "state_quota":
					message = ErrStateQuota.Error()
				case "state_write_unconfirmed":
					message = ErrStateWriteUnconfirmed.Error()
				default:
					code, message = "call_budget", r.Text
				}
				thrown := vm.NewGoError(errors.New(message))
				must(thrown.Set("code", code))
				panic(thrown)
			}
			if op == frameStateStore {
				return vm.ToValue(true)
			}
			if len(r.Input) == 0 {
				return goja.Undefined()
			}
			val, err := parse(goja.Undefined(), vm.ToValue(string(r.Input)))
			if err != nil {
				vm.Interrupt(errToolChannelBroken)
				panic(vm.NewGoError(errToolChannelBroken))
			}
			return val
		}
	}
	call := vm.Get("call").ToObject(vm)
	if err := call.Set("store", bind(frameStateStore)); err != nil {
		return err
	}
	return call.Set("load", bind(frameStateLoad))
}

// writeStateFrame measures and writes the same encoding without HTML escapes.
func writeStateFrame(w io.Writer, f Frame) error {
	data, err := toolindex.Encode(f)
	if err != nil {
		return err
	}
	if len(data) > stateFrameCap {
		return errors.New("scripthost: state frame exceeds the wire bound")
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (b *toolBridge) stateTrip(f Frame) (Frame, error) {
	if b.spent {
		return budgetFrame(b.nextID, b.limits.ToolCalls), nil
	}
	b.nextID++
	f.ID = b.nextID
	if err := writeStateFrame(b.out, f); err != nil {
		return Frame{}, fmt.Errorf("%w: writing state request: %v", errToolChannelBroken, err)
	}
	line, err := readFrameLine(b.in, stateFrameCap)
	if err != nil {
		return Frame{}, fmt.Errorf("%w: reading state reply: %v", errToolChannelBroken, err)
	}
	var r Frame
	if err := json.Unmarshal(line, &r); err != nil || r.ID != f.ID || r.Remaining < 0 || r.Remaining > b.remaining {
		return Frame{}, fmt.Errorf("%w: invalid state reply", errToolChannelBroken)
	}
	switch r.Type {
	case FrameBudget:
		if r.Remaining != 0 || !r.IsError || len(r.Input) != 0 || r.StateCode != "" {
			return Frame{}, fmt.Errorf("%w: invalid state budget reply", errToolChannelBroken)
		}
	case frameStateResult:
		switch r.StateCode {
		case "", "state_unavailable", "state_invalid", "state_quota":
		case "state_write_unconfirmed":
			if f.Type != frameStateStore {
				return Frame{}, fmt.Errorf("%w: write outcome for a state load", errToolChannelBroken)
			}
		default:
			return Frame{}, fmt.Errorf("%w: unknown state reply code", errToolChannelBroken)
		}
		if len(r.Input) != 0 && (f.Type != frameStateLoad || r.StateCode != "" || !validStateValue(r.Input)) {
			return Frame{}, fmt.Errorf("%w: invalid state value reply", errToolChannelBroken)
		}
	default:
		return Frame{}, fmt.Errorf("%w: unexpected state reply type", errToolChannelBroken)
	}
	b.remaining = r.Remaining
	b.spent = r.Remaining == 0
	return r, nil
}
