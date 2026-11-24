package valuecodec

import (
	"context"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"unicode/utf8"
)

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	jsonNumberType    = reflect.TypeFor[json.Number]()
)

type valueIdentity struct {
	typ    reflect.Type
	ptr    uintptr
	length int
}
type encodeWork struct {
	value    reflect.Value
	target   []any
	slot     int
	leaving  bool
	identity valueIdentity
}

// EncodeValue converts a JSON data tree to a flat exact frame. Native integer
// and float kinds retain their type; aliases, containers and transparent
// pointers normalize to JSON shapes. Byte slices use a distinct base64 node.
// Nil containers normalize to null; empty containers remain empty. Shared
// subtrees are copied by occurrence, while cycles on the active path fail.
// Structs and custom JSON/Text codecs are refused without invoking callbacks;
// callers may instead supply an explicitly decoded JSON value.
func EncodeValue(ctx context.Context, value any) ([]any, error) {
	nodes := make([]any, 0)
	root := make([]any, 1)
	stack := []encodeWork{{value: reflect.ValueOf(value), target: root}}
	// Values keep their owners alive while pointer identities are compared.
	active := make(map[valueIdentity]reflect.Value)
	fail := func(reason string) ([]any, error) { return nil, fmt.Errorf("%w: %s", ErrUnsupportedValue, reason) }
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if w.leaving {
			delete(active, w.identity)
			continue
		}
		v := w.value
		for v.IsValid() && v.Kind() == reflect.Interface {
			if v.IsNil() {
				v = reflect.Value{}
				break
			}
			v = v.Elem()
		}
		if v.IsValid() {
			typ := v.Type()
			if hasCustomCodec(typ) {
				return fail("custom JSON/Text codec; supply a decoded JSON value")
			}
			switch v.Kind() {
			case reflect.Pointer, reflect.Map, reflect.Slice:
				if v.IsNil() {
					v = reflect.Value{}
					break
				}
				key := valueIdentity{typ: typ, ptr: uintptr(v.UnsafePointer())}
				if v.Kind() == reflect.Slice {
					key.length = v.Len()
				}
				if _, ok := active[key]; ok {
					return fail("cyclic value")
				}
				active[key] = v
				stack = append(stack, encodeWork{leaving: true, identity: key})
				if v.Kind() == reflect.Pointer {
					stack = append(stack, encodeWork{value: v.Elem(), target: w.target, slot: w.slot})
					continue
				}
			}
		}
		id := len(nodes)
		nodes = append(nodes, nil)
		w.target[w.slot] = id
		if !v.IsValid() {
			nodes[id] = []any{"null", nil}
			continue
		}
		if v.Type() == jsonNumberType {
			text := v.String()
			if !validNumber(text) {
				return fail("invalid JSON number")
			}
			nodes[id] = []any{"number", text}
			continue
		}
		switch v.Kind() {
		case reflect.Bool:
			nodes[id] = []any{"bool", v.Bool()}
		case reflect.String:
			text := v.String()
			if !utf8.ValidString(text) {
				return fail("invalid UTF-8 string")
			}
			nodes[id] = []any{"string", text}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			nodes[id] = []any{v.Kind().String(), strconv.FormatInt(v.Int(), 10)}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			nodes[id] = []any{v.Kind().String(), strconv.FormatUint(v.Uint(), 10)}
		case reflect.Float32, reflect.Float64:
			n := v.Float()
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return fail("non-finite float")
			}
			nodes[id] = []any{v.Kind().String(), strconv.FormatFloat(n, 'g', -1, v.Type().Bits())}
		case reflect.Array, reflect.Slice:
			if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
				if v.Len() > 0 && hasCustomCodec(v.Type().Elem()) {
					return fail("custom byte-element codec; supply a decoded JSON value")
				}
				// Copy before encoding; aliases have the same binary representation.
				data := make([]byte, v.Len())
				for i := range data {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					data[i] = byte(v.Index(i).Uint())
				}
				nodes[id] = []any{"bytes", base64.StdEncoding.EncodeToString(data)}
				continue
			}
			children := make([]any, v.Len())
			nodes[id] = []any{"array", children}
			for i := v.Len() - 1; i >= 0; i-- {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				stack = append(stack, encodeWork{value: v.Index(i), target: children, slot: i})
			}
		case reflect.Map:
			type entry struct {
				key   string
				value reflect.Value
			}
			entries := make([]entry, 0, v.Len())
			seen := make(map[string]bool, v.Len())
			iter := v.MapRange()
			for iter.Next() {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				key := iter.Key()
				typ := key.Type()
				if hasCustomCodec(typ) {
					return fail("custom map-key codec")
				}
				var text string
				switch key.Kind() {
				case reflect.String:
					text = key.String()
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					text = strconv.FormatInt(key.Int(), 10)
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
					text = strconv.FormatUint(key.Uint(), 10)
				default:
					return fail("map key is not string or integer")
				}
				if !utf8.ValidString(text) {
					return fail("invalid UTF-8 object key")
				}
				if seen[text] {
					return fail("duplicate normalized object key")
				}
				seen[text] = true
				entries = append(entries, entry{text, iter.Value()})
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			pairs := make([]any, len(entries))
			nodes[id] = []any{"object", pairs}
			for i := len(entries) - 1; i >= 0; i-- {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				pair := []any{entries[i].key, nil}
				pairs[i] = pair
				stack = append(stack, encodeWork{value: entries[i].value, target: pair, slot: 1})
			}
		default:
			return fail("value is not a JSON data tree")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []any{Version, nodes}, nil
}

func validNumber(text string) bool {
	if len(text) == 0 || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) || text[len(text)-1] < '0' || text[len(text)-1] > '9' {
		return false
	}
	return json.Valid([]byte(text))
}

func hasCustomCodec(typ reflect.Type) bool {
	return typ.Implements(jsonMarshalerType) || typ.Implements(textMarshalerType) ||
		(typ.Kind() != reflect.Pointer && (reflect.PointerTo(typ).Implements(jsonMarshalerType) || reflect.PointerTo(typ).Implements(textMarshalerType)))
}
