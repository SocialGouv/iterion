package expr

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func (st *evalState) scalarText(v any) (string, error) {
	if s, ok := v.(string); ok {
		if err := st.debit(len(s)); err != nil {
			return "", err
		}
		return s, nil
	}
	_, ok, err := st.readNumber(v)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("expr: cannot concatenate %T with string", v)
	}
	if n, ok := v.(json.Number); ok {
		return string(n), nil
	}
	if err := st.debit(24); err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", v), nil // only built-in numeric types reach this call
}

func (st *evalState) elements(v any) ([]any, error) {
	if v != nil {
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			if err := st.debit(rv.Len()); err != nil {
				return nil, err
			}
		}
	}
	return toElemSlice(v)
}

// display retains the collection builtins' textual order without calling
// arbitrary formatting methods. Each leaf is charged before reading/writing
// it, including numbers nested in a collection. The explicit stack rejects
// cycles but permits shared DAG values; there is no imposed depth limit.
func (st *evalState) display(value any) (string, error) {
	type identity struct {
		typ    reflect.Type
		ptr    uintptr
		length int
	}
	type frame struct {
		value   any
		text    string
		literal bool
		leave   *identity
	}
	stack := []frame{{value: value}}
	// Keep owners alive while their pointer identities are on the active path.
	active := make(map[identity]reflect.Value)
	var out strings.Builder
	for len(stack) > 0 {
		if err := st.numberContext().Err(); err != nil {
			return "", err
		}
		if err := st.debit(1); err != nil {
			return "", err
		}
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.leave != nil {
			delete(active, *f.leave)
			continue
		}
		if f.literal {
			if err := st.debit(len(f.text)); err != nil {
				return "", err
			}
			out.WriteString(f.text)
			continue
		}
		switch v := f.value.(type) {
		case nil:
			stack = append(stack, frame{text: "<nil>", literal: true})
			continue
		case string:
			stack = append(stack, frame{text: v, literal: true})
			continue
		case bool:
			text := "false"
			if v {
				text = "true"
			}
			stack = append(stack, frame{text: text, literal: true})
			continue
		}
		if _, ok, err := st.readNumber(f.value); err != nil {
			return "", err
		} else if ok {
			text, err := st.scalarText(f.value)
			if err != nil {
				return "", err
			}
			out.WriteString(text)
			continue
		}
		rv := reflect.ValueOf(f.value)
		switch rv.Kind() {
		case reflect.Map, reflect.Slice, reflect.Array:
			if err := st.debit(rv.Len()); err != nil {
				return "", err
			}
			if rv.Kind() != reflect.Array && !rv.IsNil() {
				id := identity{typ: rv.Type(), ptr: uintptr(rv.UnsafePointer())}
				if rv.Kind() == reflect.Slice {
					id.length = rv.Len()
				}
				if _, cyclic := active[id]; cyclic {
					return "", fmt.Errorf("expr: cyclic collection cannot be formatted")
				}
				active[id] = rv
				stack = append(stack, frame{leave: &id})
			}
			stack = append(stack, frame{text: "]", literal: true})
			if rv.Kind() == reflect.Map {
				if rv.Type().Key().Kind() != reflect.String {
					return "", fmt.Errorf("expr: formatted map keys must be strings")
				}
				keys := rv.MapKeys()
				for _, k := range keys {
					if err := st.debit(len(k.String())); err != nil {
						return "", err
					}
				}
				sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
				for i := len(keys) - 1; i >= 0; i-- {
					k := keys[i]
					stack = append(stack, frame{value: rv.MapIndex(k).Interface()}, frame{text: ":", literal: true}, frame{text: k.String(), literal: true})
					if i > 0 {
						stack = append(stack, frame{text: " ", literal: true})
					}
				}
				stack = append(stack, frame{text: "map[", literal: true})
			} else {
				for i := rv.Len() - 1; i >= 0; i-- {
					stack = append(stack, frame{value: rv.Index(i).Interface()})
					if i > 0 {
						stack = append(stack, frame{text: " ", literal: true})
					}
				}
				stack = append(stack, frame{text: "[", literal: true})
			}
		default:
			return "", fmt.Errorf("expr: cannot format non-materialized value %T", f.value)
		}
	}
	return out.String(), nil
}
