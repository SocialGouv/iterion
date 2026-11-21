package recordcodec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
)

func (p *Plan) EncodeJSON(ctx context.Context, src any) ([]byte, error) {
	v, err := p.source(src)
	if err != nil {
		return nil, err
	}
	masked, m, err := prepare(ctx, p.root, v)
	if err != nil {
		return nil, err
	}
	if err = checkMetadata(ctx, masked, "json"); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(masked.Addr().Interface())
	if err != nil {
		return nil, err
	}
	out, _, err := jsonTree(ctx, p.root, raw, m, true)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
func (p *Plan) DecodeJSON(ctx context.Context, raw []byte, dst any) error {
	dest, err := p.destination(dst)
	if err != nil {
		return err
	}
	masked, m, err := jsonTree(ctx, p.root, raw, nil, false)
	if err != nil {
		return err
	}
	tmp := reflect.New(p.root.typ)
	if err = json.Unmarshal(masked, tmp.Interface()); err != nil {
		return err
	}
	if err = materialize(ctx, p.root, tmp.Elem(), m); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	dest.Set(tmp.Elem())
	return nil
}
func isNull(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
func nullable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		return true
	}
	return false
}

// objectJSON checks the selected structural envelope only. Unknown metadata
// values remain raw ordinary JSON; they are never recognized as exact frames.
func objectJSON(ctx context.Context, raw []byte, n *node) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, invalid("structural object required")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		start := dec.InputOffset()
		tok, err = dec.Token()
		if err != nil {
			return nil, err
		}
		end := dec.InputOffset()
		key, ok := tok.(string)
		if !ok {
			return nil, invalid("object key required")
		}
		keyRaw := bytes.TrimSpace(raw[start:end])
		keyRaw = bytes.TrimSpace(bytes.TrimPrefix(keyRaw, []byte{','}))
		// Token alone would silently repair invalid UTF-8 or lone surrogates.
		if _, err = valuecodec.DecodeJSON(ctx, keyRaw); err != nil {
			return nil, err
		}
		if _, exists := out[key]; exists {
			return nil, invalid("duplicate structural JSON key " + key)
		}
		for _, canonical := range n.jsonNames {
			if key != canonical && strings.EqualFold(key, canonical) {
				return nil, invalid("noncanonical structural JSON key " + key)
			}
		}
		var val json.RawMessage
		if err = dec.Decode(&val); err != nil {
			return nil, err
		}
		out[key] = val
	}
	if _, err = dec.Token(); err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, invalid("trailing JSON value")
	}
	return out, nil
}
func jsonTree(ctx context.Context, n *node, raw []byte, m *material, encode bool) (json.RawMessage, *material, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if n.payload {
		if encode {
			b, err := json.Marshal(m.value)
			return b, m, err
		}
		value, err := valuecodec.DecodeFrameJSON(ctx, raw)
		if err != nil {
			return nil, nil, err
		}
		return json.RawMessage("null"), &material{value: value}, nil
	}
	if isNull(raw) {
		if !nullable(n.typ) {
			return nil, nil, invalid("null structural record")
		}
		return raw, nil, nil
	}
	t := base(n.typ)
	if !encode {
		m = &material{}
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, err := objectJSON(ctx, raw, n)
		if err != nil {
			return nil, nil, err
		}
		if !encode {
			m.fields = map[int]*material{}
		}
		for _, f := range n.fields {
			child, present := obj[f.json]
			if encode && !present && !f.child.payload && m.fields[f.index] != nil {
				switch base(f.child.typ).Kind() {
				case reflect.Map:
					child = json.RawMessage(`{}`)
					present = true
				case reflect.Slice, reflect.Array:
					child = json.RawMessage(`[]`)
					present = true
				}
			}
			if !present {
				if f.child.payload {
					if !encode {
						return nil, nil, invalid("missing payload " + f.json)
					}
				} else {
					// A nil/empty parent omitted by the ordinary codec needs no new object.
					// Non-nilable structs cannot be skipped by a malicious exact record.
					if nullable(f.child.typ) {
						continue
					}
					return nil, nil, invalid("missing structural record " + f.json)
				}
			}
			var cm *material
			if encode {
				cm = m.fields[f.index]
			}
			next, got, err := jsonTree(ctx, f.child, child, cm, encode)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", f.json, err)
			}
			obj[f.json] = next
			if !encode {
				m.fields[f.index] = got
			}
		}
		b, err := json.Marshal(obj)
		return b, m, err
	case reflect.Map:
		obj, err := objectJSON(ctx, raw, n)
		if err != nil {
			return nil, nil, err
		}
		if !encode {
			m.keys = map[string]*material{}
		}
		for k, child := range obj {
			var cm *material
			if encode {
				cm = m.keys[k]
			}
			next, got, err := jsonTree(ctx, n.elem, child, cm, encode)
			if err != nil {
				return nil, nil, err
			}
			obj[k] = next
			if !encode {
				m.keys[k] = got
			}
		}
		b, err := json.Marshal(obj)
		return b, m, err
	case reflect.Slice, reflect.Array:
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, nil, err
		}
		if t.Kind() == reflect.Array && len(arr) != t.Len() {
			return nil, nil, invalid("wrong structural array length")
		}
		if !encode {
			m.items = make([]*material, len(arr))
		}
		for i, child := range arr {
			var cm *material
			if encode {
				cm = m.items[i]
			}
			next, got, err := jsonTree(ctx, n.elem, child, cm, encode)
			if err != nil {
				return nil, nil, err
			}
			arr[i] = next
			if !encode {
				m.items[i] = got
			}
		}
		b, err := json.Marshal(arr)
		return b, m, err
	}
	return nil, nil, invalid("invalid structural plan")
}
