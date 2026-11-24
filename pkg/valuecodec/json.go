// Package valuecodec preserves structured values across explicitly versioned
// transports. It does not infer formats from user objects or validate schemas.
package valuecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// ErrInvalidJSON identifies malformed JSON or text that a standard decoder
// would silently replace. No partially decoded value accompanies this error.
var ErrInvalidJSON = errors.New("invalid exact JSON")

// DecodeJSON reads one JSON value without rounding numbers or silently replacing
// invalid Unicode. Object keys must be unique after JSON escape decoding. It
// accepts every JSON value kind; a caller owns any root-shape or schema rule.
// The standard token decoder retains ownership of syntax; containers are built
// iteratively without imposing an additional nesting limit.
func DecodeJSON(ctx context.Context, data []byte) (any, error) {
	if err := checkJSONUnicode(ctx, data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	type frame struct {
		kind   json.Delim
		object map[string]any
		array  []any
		key    string
		hasKey bool
	}
	var stack []frame
	var result any
	finished := false
	attach := func(value any) error {
		if len(stack) == 0 {
			if finished {
				return fmt.Errorf("%w: more than one value", ErrInvalidJSON)
			}
			result, finished = value, true
			return nil
		}
		parent := &stack[len(stack)-1]
		if parent.kind == '[' {
			parent.array = append(parent.array, value)
			return nil
		}
		if !parent.hasKey {
			return fmt.Errorf("%w: object value has no key", ErrInvalidJSON)
		}
		parent.object[parent.key] = value
		parent.key, parent.hasKey = "", false
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			if !finished || len(stack) != 0 {
				return nil, fmt.Errorf("%w: incomplete value", ErrInvalidJSON)
			}
			return result, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
		}
		if finished {
			return nil, fmt.Errorf("%w: more than one value", ErrInvalidJSON)
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				stack = append(stack, frame{kind: delim, object: make(map[string]any)})
			case '[':
				stack = append(stack, frame{kind: delim, array: make([]any, 0)})
			case '}', ']':
				if len(stack) == 0 {
					return nil, fmt.Errorf("%w: unmatched delimiter", ErrInvalidJSON)
				}
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				var value any
				if delim == '}' && top.kind == '{' && !top.hasKey {
					value = top.object
				} else if delim == ']' && top.kind == '[' {
					value = top.array
				} else {
					return nil, fmt.Errorf("%w: mismatched delimiter", ErrInvalidJSON)
				}
				if err := attach(value); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("%w: invalid delimiter", ErrInvalidJSON)
			}
			continue
		}
		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			if top.kind == '{' && !top.hasKey {
				key, ok := token.(string)
				if !ok {
					return nil, fmt.Errorf("%w: non-string object key", ErrInvalidJSON)
				}
				if _, exists := top.object[key]; exists {
					return nil, fmt.Errorf("%w: duplicate object key", ErrInvalidJSON)
				}
				top.key, top.hasKey = key, true
				continue
			}
		}
		if err := attach(token); err != nil {
			return nil, err
		}
	}
}

// checkJSONUnicode validates fidelity only; encoding/json owns JSON syntax.
func checkJSONUnicode(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("%w: invalid UTF-8", ErrInvalidJSON)
	}
	inString := false
	for i := 0; i < len(data); i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) || data[i] != 'u' || i+4 >= len(data) {
			continue
		}
		unit, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			continue
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return fmt.Errorf("%w: unpaired low surrogate", ErrInvalidJSON)
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
			return fmt.Errorf("%w: unpaired high surrogate", ErrInvalidJSON)
		}
		low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return fmt.Errorf("%w: unpaired high surrogate", ErrInvalidJSON)
		}
		i += 6
	}
	return ctx.Err()
}
