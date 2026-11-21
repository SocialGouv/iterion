package recordcodec

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func (p *Plan) EncodeBSON(ctx context.Context, src any) ([]byte, error) {
	v, err := p.source(src)
	if err != nil {
		return nil, err
	}
	masked, m, err := prepare(ctx, p.root, v)
	if err != nil {
		return nil, err
	}
	if err = checkMetadata(ctx, masked, "bson"); err != nil {
		return nil, err
	}
	raw, err := bson.Marshal(masked.Addr().Interface())
	if err != nil {
		return nil, err
	}
	var doc bson.D
	if err = bson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out, _, err := bsonTree(ctx, p.root, doc, m, true)
	if err != nil {
		return nil, err
	}
	raw, err = bson.Marshal(out)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}
func (p *Plan) DecodeBSON(ctx context.Context, raw []byte, dst any) error {
	dest, err := p.destination(dst)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = bson.Raw(raw).Validate(); err != nil {
		return err
	}
	var doc bson.D
	if err = bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	masked, m, err := bsonTree(ctx, p.root, doc, nil, false)
	if err != nil {
		return err
	}
	bytes, err := bson.Marshal(masked)
	if err != nil {
		return err
	}
	tmp := reflect.New(p.root.typ)
	if err = bson.Unmarshal(bytes, tmp.Interface()); err != nil {
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
func objectBSON(value any) (bson.D, map[string]int, error) {
	doc, ok := value.(bson.D)
	if !ok {
		return nil, nil, invalid("structural BSON document required")
	}
	indices := make(map[string]int, len(doc))
	for i, e := range doc {
		if !utf8.ValidString(e.Key) {
			return nil, nil, invalid("invalid BSON key")
		}
		if _, ok := indices[e.Key]; ok {
			return nil, nil, invalid("duplicate structural BSON key " + e.Key)
		}
		indices[e.Key] = i
	}
	return doc, indices, nil
}
func bsonTree(ctx context.Context, n *node, value any, m *material, encode bool) (any, *material, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if n.payload {
		if encode {
			return m.value, m, nil
		}
		v, err := valuecodec.DecodeValue(ctx, value)
		if err != nil {
			return nil, nil, err
		}
		return nil, &material{value: v}, nil
	}
	if value == nil {
		if !nullable(n.typ) {
			return nil, nil, invalid("null structural record")
		}
		return nil, nil, nil
	}
	t := base(n.typ)
	if !encode {
		m = &material{}
	}
	switch t.Kind() {
	case reflect.Struct:
		doc, idx, err := objectBSON(value)
		if err != nil {
			return nil, nil, err
		}
		for key := range idx {
			for _, canonical := range n.bsonNames {
				if key != canonical && strings.EqualFold(key, canonical) {
					return nil, nil, invalid("noncanonical structural BSON key " + key)
				}
			}
		}
		if !encode {
			m.fields = map[int]*material{}
		}
		for _, f := range n.fields {
			pos, present := idx[f.bson]
			var child any
			if present {
				child = doc[pos].Value
			} else if encode && !f.child.payload && m.fields[f.index] != nil {
				switch base(f.child.typ).Kind() {
				case reflect.Map:
					child = bson.D{}
				case reflect.Slice, reflect.Array:
					child = bson.A{}
				}
			} else if !f.child.payload {
				if nullable(f.child.typ) {
					continue
				}
				return nil, nil, invalid("missing structural record " + f.bson)
			} else if !encode {
				return nil, nil, invalid("missing payload " + f.bson)
			}
			var cm *material
			if encode {
				cm = m.fields[f.index]
			}
			next, got, err := bsonTree(ctx, f.child, child, cm, encode)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", f.bson, err)
			}
			if present {
				doc[pos].Value = next
			} else {
				doc = append(doc, bson.E{Key: f.bson, Value: next})
			}
			if !encode {
				m.fields[f.index] = got
			}
		}
		return doc, m, nil
	case reflect.Map:
		doc, _, err := objectBSON(value)
		if err != nil {
			return nil, nil, err
		}
		if !encode {
			m.keys = map[string]*material{}
		}
		for i, e := range doc {
			var cm *material
			if encode {
				cm = m.keys[e.Key]
			}
			next, got, err := bsonTree(ctx, n.elem, e.Value, cm, encode)
			if err != nil {
				return nil, nil, err
			}
			doc[i].Value = next
			if !encode {
				m.keys[e.Key] = got
			}
		}
		return doc, m, nil
	case reflect.Slice, reflect.Array:
		arr, ok := value.(bson.A)
		if !ok {
			return nil, nil, invalid("structural BSON array required")
		}
		if t.Kind() == reflect.Array && len(arr) != t.Len() {
			return nil, nil, invalid("wrong structural array length")
		}
		if !encode {
			m.items = make([]*material, len(arr))
		}
		for i, e := range arr {
			var cm *material
			if encode {
				cm = m.items[i]
			}
			next, got, err := bsonTree(ctx, n.elem, e, cm, encode)
			if err != nil {
				return nil, nil, err
			}
			arr[i] = next
			if !encode {
				m.items[i] = got
			}
		}
		return arr, m, nil
	}
	return nil, nil, invalid("invalid structural plan")
}
