// Package recordcodec adapts explicitly selected record fields to exact-value
// frames. It neither chooses a persistence version nor enables any writer.
package recordcodec

import (
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var ErrInvalidRecord = errors.New("invalid exact record")

// Codec operates on one precise Go record type. Implementations publish a
// decoded value only after the entire record has passed validation. Contexts
// are checked in adapter traversals and before publication. Ordinary metadata
// codecs retain their own synchronous callback contract; the adapter cannot
// interrupt a callback while it is executing. Payload callbacks are refused.
type Codec interface {
	EncodeJSON(context.Context, any) ([]byte, error)
	DecodeJSON(context.Context, []byte, any) error
	EncodeBSON(context.Context, any) ([]byte, error)
	DecodeBSON(context.Context, []byte, any) error
}

// Plan is immutable and safe for concurrent use. Paths are Go field names,
// separated by '/', with '*' for map values and '[]' for list elements.
// Maps inside a payload are data, not additional structural paths.
type Plan struct{ root *node }
type node struct {
	typ       reflect.Type
	payload   bool
	fields    []field
	elem      *node
	jsonNames []string
	bsonNames []string
}
type field struct {
	index      int
	json, bson string
	child      *node
}
type material struct {
	value  any
	fields map[int]*material
	keys   map[string]*material
	items  []*material
}

// Compile validates the complete explicit registry before processing a record.
// Structural embedding, custom codecs and tag options which change shape are
// deliberately unsupported. Off-path metadata retains its ordinary codecs.
func Compile(typ reflect.Type, paths ...string) (*Plan, error) {
	if typ == nil || typ.Kind() != reflect.Struct || len(paths) == 0 {
		return nil, invalid("plan requires a struct and payload paths")
	}
	root := &node{typ: typ}
	for _, path := range paths {
		if path == "" {
			return nil, invalid("empty path")
		}
		if err := root.add(strings.Split(path, "/")); err != nil {
			return nil, fmt.Errorf("path %s: %w", path, err)
		}
	}
	return &Plan{root: root}, nil
}
func invalid(s string) error { return fmt.Errorf("%w: %s", ErrInvalidRecord, s) }

// base returns nil when a pointer-only cycle has no structural record.
func base(t reflect.Type) reflect.Type {
	seen := map[reflect.Type]bool{}
	for t.Kind() == reflect.Pointer {
		if seen[t] {
			return nil
		}
		seen[t] = true
		t = t.Elem()
	}
	return t
}

var hookTypes = []reflect.Type{
	reflect.TypeFor[json.Marshaler](), reflect.TypeFor[json.Unmarshaler](),
	reflect.TypeFor[encoding.TextMarshaler](), reflect.TypeFor[encoding.TextUnmarshaler](),
	reflect.TypeFor[bson.Marshaler](), reflect.TypeFor[bson.Unmarshaler](),
	reflect.TypeFor[bson.ValueMarshaler](), reflect.TypeFor[bson.ValueUnmarshaler](),
}

func hasHook(t reflect.Type) bool {
	for _, h := range hookTypes {
		if t.Implements(h) || reflect.PointerTo(t).Implements(h) {
			return true
		}
	}
	return false
}

// payloadType follows map element types to their required any terminal.
// Revisiting a type cannot reach that terminal; finite nested maps remain valid.
func payloadType(t reflect.Type) bool {
	seen := map[reflect.Type]bool{}
	for {
		if seen[t] || hasHook(t) || t.Kind() != reflect.Map || t.Key().Kind() != reflect.String || hasHook(t.Key()) {
			return false
		}
		seen[t] = true
		t = t.Elem()
		if t.Kind() == reflect.Interface && t.NumMethod() == 0 {
			return true
		}
	}
}

func names(f reflect.StructField) (string, string, error) {
	if f.Anonymous {
		return "", "", invalid("embedded structural fields are unsupported")
	}
	j, b := f.Name, strings.ToLower(f.Name)
	for _, key := range []string{"json", "bson"} {
		tag, ok := f.Tag.Lookup(key)
		if !ok {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name != "" {
			if key == "json" {
				j = name
			} else {
				b = name
			}
		}
		for _, opt := range parts[1:] {
			if opt == "inline" {
				return "", "", invalid("unsupported structural tag option " + opt)
			}
		}
	}
	if !utf8.ValidString(j) || !utf8.ValidString(b) || strings.ContainsRune(b, 0) {
		return "", "", invalid("invalid field tag")
	}
	// encoding/json ignores invalid tag names; accepting them here would mask a
	// different field from the one the ordinary serializer actually emits.
	for _, c := range j {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c) {
			return "", "", invalid("invalid JSON field tag")
		}
	}
	return j, b, nil
}
func (n *node) add(path []string) error {
	if n.payload {
		return invalid("duplicate or overlapping payload path")
	}
	if len(path) == 0 {
		if n.elem != nil || len(n.fields) > 0 || !payloadType(n.typ) {
			return invalid("payload must be a string-keyed map ending in any")
		}
		n.payload = true
		return nil
	}
	t := base(n.typ)
	if t == nil {
		return invalid("recursive pointer-only structural type")
	}
	if hasHook(n.typ) || hasHook(t) {
		return invalid("custom codec on structural ancestor")
	}
	switch t.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array:
		token := "[]"
		if t.Kind() == reflect.Map {
			token = "*"
			if t.Key().Kind() != reflect.String || hasHook(t.Key()) {
				return invalid("structural map requires string keys")
			}
		}
		if path[0] != token {
			return invalid("expected " + token)
		}
		if n.elem == nil {
			n.elem = &node{typ: t.Elem()}
		}
		return n.elem.add(path[1:])
	case reflect.Struct:
		if n.jsonNames == nil {
			js, bs := map[string]bool{}, map[string]bool{}
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if !f.IsExported() {
					continue
				}
				j, b, err := names(f)
				if err != nil {
					return err
				}
				if j != "-" {
					for prev := range js {
						if strings.EqualFold(j, prev) {
							return invalid("ambiguous JSON field names")
						}
					}
					js[j] = true
					n.jsonNames = append(n.jsonNames, j)
				}
				if b != "-" {
					for prev := range bs {
						if strings.EqualFold(b, prev) {
							return invalid("ambiguous BSON field names")
						}
					}
					bs[b] = true
					n.bsonNames = append(n.bsonNames, b)
				}
			}
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Name != path[0] {
				continue
			}
			if !f.IsExported() {
				return invalid("unexported field")
			}
			j, b, err := names(f)
			if err != nil {
				return err
			}
			if j == "-" || b == "-" {
				return invalid("payload path excluded by tag")
			}
			for _, cf := range n.fields {
				if cf.index == i {
					return cf.child.add(path[1:])
				}
			}
			for _, key := range []string{"json", "bson"} {
				for _, opt := range strings.Split(f.Tag.Get(key), ",")[1:] {
					if opt != "" && opt != "omitempty" {
						return invalid("unsupported selected tag option " + opt)
					}
				}
			}
			child := &node{typ: f.Type}
			if err = child.add(path[1:]); err != nil {
				return err
			}
			n.fields = append(n.fields, field{i, j, b, child})
			sort.Slice(n.fields, func(i, j int) bool { return n.fields[i].index < n.fields[j].index })
			return nil
		}
		return invalid("unknown field " + path[0])
	default:
		return invalid("path traverses scalar")
	}
}
func (p *Plan) source(src any) (reflect.Value, error) {
	if p == nil || p.root == nil {
		return reflect.Value{}, invalid("nil plan")
	}
	v := reflect.ValueOf(src)
	if v.IsValid() && v.Type() == reflect.PointerTo(p.root.typ) && !v.IsNil() {
		v = v.Elem()
	}
	if !v.IsValid() || v.Type() != p.root.typ {
		return reflect.Value{}, invalid("wrong source type")
	}
	return v, nil
}
func (p *Plan) destination(dst any) (reflect.Value, error) {
	if p == nil || p.root == nil {
		return reflect.Value{}, invalid("nil plan")
	}
	v := reflect.ValueOf(dst)
	if !v.IsValid() || v.Type() != reflect.PointerTo(p.root.typ) || v.IsNil() {
		return reflect.Value{}, invalid("wrong destination type")
	}
	return v.Elem(), nil
}

// prepare copies the shared structural tree once and masks before any ordinary
// serializer can see a payload (including unsupported BSON json.Number values).
func prepare(ctx context.Context, n *node, v reflect.Value) (reflect.Value, *material, error) {
	if err := ctx.Err(); err != nil {
		return reflect.Value{}, nil, err
	}
	if n.payload {
		f, err := valuecodec.EncodeValue(ctx, v.Interface())
		return reflect.Zero(v.Type()), &material{value: f}, err
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return v, nil, nil
		}
		x, m, err := prepare(ctx, &node{typ: v.Type().Elem(), fields: n.fields, elem: n.elem, jsonNames: n.jsonNames, bsonNames: n.bsonNames}, v.Elem())
		if err != nil {
			return reflect.Value{}, nil, err
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(x)
		return out, m, nil
	}
	out := reflect.New(v.Type()).Elem()
	out.Set(v)
	m := &material{}
	switch v.Kind() {
	case reflect.Struct:
		m.fields = map[int]*material{}
		for _, f := range n.fields {
			x, c, err := prepare(ctx, f.child, v.Field(f.index))
			if err != nil {
				return reflect.Value{}, nil, fmt.Errorf("%s: %w", f.json, err)
			}
			out.Field(f.index).Set(x)
			m.fields[f.index] = c
		}
	case reflect.Map:
		if v.IsNil() {
			return v, nil, nil
		}
		out = reflect.MakeMapWithSize(v.Type(), v.Len())
		m.keys = map[string]*material{}
		it := v.MapRange()
		for it.Next() {
			k := it.Key()
			if !utf8.ValidString(k.String()) || strings.ContainsRune(k.String(), 0) {
				return reflect.Value{}, nil, invalid("invalid structural map key")
			}
			x, c, err := prepare(ctx, n.elem, it.Value())
			if err != nil {
				return reflect.Value{}, nil, err
			}
			out.SetMapIndex(k, x)
			m.keys[k.String()] = c
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice {
			if v.IsNil() {
				return v, nil, nil
			}
			out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		}
		m.items = make([]*material, v.Len())
		for i := 0; i < v.Len(); i++ {
			x, c, err := prepare(ctx, n.elem, v.Index(i))
			if err != nil {
				return reflect.Value{}, nil, err
			}
			out.Index(i).Set(x)
			m.items[i] = c
		}
	}
	return out, m, nil
}
func materialize(ctx context.Context, n *node, v reflect.Value, m *material) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil {
		return nil
	}
	if n.payload {
		x, err := payloadNative(ctx, v.Type(), m.value)
		if err != nil {
			return err
		}
		v.Set(x)
		return nil
	}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return invalid("missing structural parent")
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		for _, f := range n.fields {
			if err := materialize(ctx, f.child, v.Field(f.index), m.fields[f.index]); err != nil {
				return err
			}
		}
	case reflect.Map:
		for k, c := range m.keys {
			key := reflect.ValueOf(k).Convert(v.Type().Key())
			old := v.MapIndex(key)
			if !old.IsValid() {
				return invalid("missing structural map entry")
			}
			x := reflect.New(old.Type()).Elem()
			x.Set(old)
			if err := materialize(ctx, n.elem, x, c); err != nil {
				return err
			}
			v.SetMapIndex(key, x)
		}
	case reflect.Slice, reflect.Array:
		if v.Len() != len(m.items) {
			return invalid("structural list length changed")
		}
		for i, c := range m.items {
			if err := materialize(ctx, n.elem, v.Index(i), c); err != nil {
				return err
			}
		}
	}
	return nil
}
func payloadNative(ctx context.Context, t reflect.Type, value any) (reflect.Value, error) {
	if err := ctx.Err(); err != nil {
		return reflect.Value{}, err
	}
	if value == nil {
		return reflect.Zero(t), nil
	}
	if t.Kind() == reflect.Interface {
		return reflect.ValueOf(value), nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return reflect.Value{}, invalid("payload root is not an object or null")
	}
	out := reflect.MakeMapWithSize(t, len(obj))
	for k, val := range obj {
		x, err := payloadNative(ctx, t.Elem(), val)
		if err != nil {
			return reflect.Value{}, err
		}
		out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), x)
	}
	return out, nil
}

// checkMetadata guards BSON's ordinary recursive encoder against native cycles.
// It does not reinterpret metadata or invoke user callbacks. Shared DAGs remain
// valid. Only fields included by this transport can reach its serializer.
// Payloads have already been masked and checked by valuecodec.
func checkMetadata(ctx context.Context, root reflect.Value, format string) error {
	type identity struct {
		typ    reflect.Type
		ptr    uintptr
		length int
	}
	type visit struct {
		v     reflect.Value
		leave *identity
	}
	// Retain owners while their pointer identities are on the active path.
	active := map[identity]reflect.Value{}
	stack := []visit{{v: root}}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if it.leave != nil {
			delete(active, *it.leave)
			continue
		}
		v := it.v
		if !v.IsValid() {
			continue
		}
		if v.Kind() == reflect.Interface {
			if !v.IsNil() {
				stack = append(stack, visit{v: v.Elem()})
			}
			continue
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice:
			if v.IsNil() {
				continue
			}
			if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
				continue
			}
			length := 0
			if v.Kind() == reflect.Slice {
				length = v.Len()
			}
			key := identity{v.Type(), uintptr(v.UnsafePointer()), length}
			if _, exists := active[key]; exists {
				return invalid("cyclic metadata")
			}
			active[key] = v
			stack = append(stack, visit{leave: &key})
		}
		switch v.Kind() {
		case reflect.Pointer:
			stack = append(stack, visit{v: v.Elem()})
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				stack = append(stack, visit{v: iter.Value()})
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				stack = append(stack, visit{v: v.Index(i)})
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() && v.Type().Field(i).Tag.Get(format) != "-" {
					stack = append(stack, visit{v: v.Field(i)})
				}
			}
		}
	}
	return nil
}
