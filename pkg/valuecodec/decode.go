package valuecodec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

func frameList(value any) (reflect.Value, bool) {
	v := reflect.ValueOf(value)
	if !v.IsValid() || (v.Kind() != reflect.Slice && v.Kind() != reflect.Array) {
		return reflect.Value{}, false
	}
	return v, true
}
func frameString(value any) (string, bool) {
	s, ok := value.(string)
	return s, ok && utf8.ValidString(s)
}

func invalidFrame(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidFrame, reason) }

// DecodeValue reads only an explicit exact frame. Transport node numbering can
// vary, but children must follow their parent and every non-root node has one
// parent. It builds a new tree bottom-up without recursion or shared subtrees.
// Unknown versions retain their distinct error even if their arity differs.
func DecodeValue(ctx context.Context, frame any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tuple, ok := frameList(frame)
	if !ok || tuple.Len() == 0 {
		return nil, invalidFrame("expected versioned array")
	}
	version, ok := frameString(tuple.Index(0).Interface())
	if !ok {
		return nil, invalidFrame("version is not text")
	}
	if version != Version {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedVersion, version)
	}
	if tuple.Len() != 2 {
		return nil, invalidFrame("v1 frame arity")
	}
	nodes, ok := frameList(tuple.Index(1).Interface())
	if !ok || nodes.Len() == 0 {
		return nil, invalidFrame("missing root node")
	}
	values := make([]any, nodes.Len())
	parents := make([]bool, nodes.Len())
	childValue := func(parent int, raw any) (any, error) {
		child, ok := frameIndex(raw, nodes.Len())
		if !ok || child <= parent {
			return nil, invalidFrame("child index is outside forward range")
		}
		if parents[child] {
			return nil, invalidFrame("node has multiple parents")
		}
		parents[child] = true
		return values[child], nil
	}
	for i := nodes.Len() - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node, ok := frameList(nodes.Index(i).Interface())
		if !ok || node.Len() != 2 {
			return nil, invalidFrame("node arity")
		}
		kind, ok := frameString(node.Index(0).Interface())
		if !ok {
			return nil, invalidFrame("node kind is not text")
		}
		payload := node.Index(1).Interface()
		switch kind {
		case "null":
			if payload != nil {
				return nil, invalidFrame("null payload")
			}
			values[i] = nil
		case "bool":
			v, ok := payload.(bool)
			if !ok {
				return nil, invalidFrame("bool payload")
			}
			values[i] = v
		case "string":
			v, ok := frameString(payload)
			if !ok {
				return nil, invalidFrame("string payload")
			}
			values[i] = v
		case "object":
			pairs, ok := frameList(payload)
			if !ok {
				return nil, invalidFrame("object payload")
			}
			object := make(map[string]any, pairs.Len())
			for j := 0; j < pairs.Len(); j++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				pair, ok := frameList(pairs.Index(j).Interface())
				if !ok || pair.Len() != 2 {
					return nil, invalidFrame("object pair arity")
				}
				key, ok := frameString(pair.Index(0).Interface())
				if !ok {
					return nil, invalidFrame("object key")
				}
				if _, exists := object[key]; exists {
					return nil, invalidFrame("duplicate object key")
				}
				child, err := childValue(i, pair.Index(1).Interface())
				if err != nil {
					return nil, err
				}
				object[key] = child
			}
			values[i] = object
		case "array":
			raw, ok := frameList(payload)
			if !ok {
				return nil, invalidFrame("array payload")
			}
			array := make([]any, raw.Len())
			for j := 0; j < raw.Len(); j++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				child, err := childValue(i, raw.Index(j).Interface())
				if err != nil {
					return nil, err
				}
				array[j] = child
			}
			values[i] = array
		case "bytes":
			text, ok := frameString(payload)
			if !ok {
				return nil, invalidFrame("binary payload")
			}
			data, err := base64.StdEncoding.Strict().DecodeString(text)
			if err != nil || base64.StdEncoding.EncodeToString(data) != text {
				return nil, invalidFrame("noncanonical binary payload")
			}
			values[i] = data
		default:
			text, ok := frameString(payload)
			if !ok {
				return nil, invalidFrame("numeric payload is not text")
			}
			value, ok := decodeNumber(kind, text)
			if !ok {
				return nil, invalidFrame("unknown kind or invalid numeric payload")
			}
			values[i] = value
		}
	}
	for i := 1; i < len(parents); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !parents[i] {
			return nil, invalidFrame("orphan node")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return values[0], nil
}

func frameIndex(raw any, limit int) (int, bool) {
	if n, ok := raw.(json.Number); ok {
		return decimalIndex(string(n), limit)
	}
	v := reflect.ValueOf(raw)
	if !v.IsValid() {
		return 0, false
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := v.Int()
		if n < 0 || uint64(n) >= uint64(limit) {
			return 0, false
		}
		return int(n), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := v.Uint()
		if n >= uint64(limit) {
			return 0, false
		}
		return int(n), true
	case reflect.Float32, reflect.Float64:
		n := v.Float()
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || math.Trunc(n) != n || n >= float64(limit) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

func decodeNumber(kind, text string) (any, bool) {
	if kind == "number" {
		if !validNumber(text) {
			return nil, false
		}
		return json.Number(text), true
	}
	signedBits := map[string]int{"int": strconv.IntSize, "int8": 8, "int16": 16, "int32": 32, "int64": 64}
	if bits, ok := signedBits[kind]; ok {
		n, err := strconv.ParseInt(text, 10, bits)
		if err != nil || strconv.FormatInt(n, 10) != text {
			return nil, false
		}
		switch kind {
		case "int":
			return int(n), true
		case "int8":
			return int8(n), true
		case "int16":
			return int16(n), true
		case "int32":
			return int32(n), true
		default:
			return n, true
		}
	}
	unsignedBits := map[string]int{"uint": strconv.IntSize, "uint8": 8, "uint16": 16, "uint32": 32, "uint64": 64, "uintptr": strconv.IntSize}
	if bits, ok := unsignedBits[kind]; ok {
		n, err := strconv.ParseUint(text, 10, bits)
		if err != nil || strconv.FormatUint(n, 10) != text {
			return nil, false
		}
		switch kind {
		case "uint":
			return uint(n), true
		case "uint8":
			return uint8(n), true
		case "uint16":
			return uint16(n), true
		case "uint32":
			return uint32(n), true
		case "uintptr":
			return uintptr(n), true
		default:
			return n, true
		}
	}
	bits := 64
	if kind == "float32" {
		bits = 32
	} else if kind != "float64" {
		return nil, false
	}
	n, err := strconv.ParseFloat(text, bits)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || strconv.FormatFloat(n, 'g', -1, bits) != text {
		return nil, false
	}
	if bits == 32 {
		return float32(n), true
	}
	return n, true
}

// decimalIndex decides integrality before any float conversion. Its work is
// bounded by the input text and the allocated node count, not by a possibly
// enormous exponent. Only zero can stay in range with an unbounded exponent.
func decimalIndex(text string, limit int) (int, bool) {
	if !validNumber(text) {
		return 0, false
	}
	negative := text[0] == '-'
	if negative {
		text = text[1:]
	}
	exponent := "0"
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, text = text[i+1:], text[:i]
	}
	fraction := 0
	if i := strings.IndexByte(text, '.'); i >= 0 {
		fraction = len(text) - i - 1
		text = text[:i] + text[i+1:]
	}
	digits := strings.TrimLeft(text, "0")
	if digits == "" {
		return 0, limit > 0
	}
	if negative {
		return 0, false
	}
	exp, err := strconv.ParseInt(exponent, 10, 64)
	if err != nil {
		return 0, false
	}
	// Guard the subtraction before computing scale. An exponent outside these
	// bounds cannot turn this nonzero mantissa into an in-range integer.
	maxDigits := len(strconv.Itoa(limit - 1))
	if exp < -int64(len(digits)) || exp > int64(fraction)+int64(maxDigits) {
		return 0, false
	}
	scale := exp - int64(fraction)
	if scale < 0 {
		remove := -scale
		if remove >= int64(len(digits)) {
			return 0, false
		}
		tail := digits[len(digits)-int(remove):]
		if strings.Trim(tail, "0") != "" {
			return 0, false
		}
		digits = digits[:len(digits)-int(remove)]
	} else {
		if int64(len(digits))+scale > int64(maxDigits) {
			return 0, false
		}
		digits += strings.Repeat("0", int(scale))
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || n >= uint64(limit) {
		return 0, false
	}
	return int(n), true
}
