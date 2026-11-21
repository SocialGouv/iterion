package valuecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Version identifies an explicit exact-value frame, never a user-object field.
const Version = "iterion.exact-values/1"

var (
	ErrUnsupportedValue = errors.New("unsupported exact value")
	ErrInvalidFrame     = errors.New("invalid exact-value frame")
	// ErrUnsupportedVersion must not be treated as tolerable record corruption.
	ErrUnsupportedVersion = errors.New("unsupported exact-value version")
)

// EncodeMap frames a store-owned map, including nil. An integration chooses the
// run's immutable format before calling this; no writer is enabled by this API.
func EncodeMap(ctx context.Context, value map[string]any) ([]any, error) {
	return EncodeValue(ctx, value)
}

// DecodeMap requires an explicit exact frame whose root is an object or null.
// It never falls back to a legacy map or recognizes metadata in user objects.
func DecodeMap(ctx context.Context, frame any) (map[string]any, error) {
	value, err := DecodeValue(ctx, frame)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: root is not an object or null", ErrInvalidFrame)
	}
	return object, nil
}

// CanonicalJSON produces the bytes used for identity: DFS preorder nodes,
// UTF-8 byte-sorted object keys, ordered arrays, and compact JSON without a final
// newline. Strings use JSON escapes for controls, quote, backslash, U+2028 and
// U+2029; other Unicode and <, >, & remain literal. Numeric payloads are strings.
// A received frame must be decoded before this call, never hashed directly.
// This is representation identity, not a rule for arithmetic equality.
func CanonicalJSON(ctx context.Context, value any) ([]byte, error) {
	frame, err := EncodeValue(ctx, value)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(frame); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

// DecodeFrameJSON preserves text before reading the explicit frame. Generic
// json.Unmarshal may already have replaced invalid Unicode before DecodeValue.
func DecodeFrameJSON(ctx context.Context, data []byte) (any, error) {
	frame, err := DecodeJSON(ctx, data)
	if err != nil {
		return nil, err
	}
	return DecodeValue(ctx, frame)
}
