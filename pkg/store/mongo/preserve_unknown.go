package mongo

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var bsonFieldTypes sync.Map // reflect.Type -> map[bson field name]reflect.Type

func bsonStructFields(typ reflect.Type) map[string]reflect.Type {
	if fields, ok := bsonFieldTypes.Load(typ); ok {
		return fields.(map[string]reflect.Type)
	}
	fields := make(map[string]reflect.Type, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag := strings.Split(field.Tag.Get("bson"), ",")
		if tag[0] == "-" {
			continue
		}
		name := tag[0]
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		fields[name] = field.Type
	}
	bsonFieldTypes.Store(typ, fields)
	return fields
}

func bsonDocument(value any) (bson.M, bool) {
	switch value := value.(type) {
	case bson.M:
		return value, true
	case map[string]any:
		return bson.M(value), true // the public store registry's document shape
	case bson.D:
		doc := make(bson.M, len(value))
		for _, element := range value {
			doc[element.Key] = element.Value
		}
		return doc, true
	default:
		return nil, false
	}
}

// preserveUnknownBSON keeps fields absent from this writer's Go schema.
// Known fields remain authoritative: omission clears an omitempty field,
// dropping a map key deletes that entry, and clearing a subtree deletes its
// extensions too. Maps of arbitrary output/inputs are payload, not schemas.
func preserveUnknownBSON(previous, replacement any, typ reflect.Type) (any, error) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct, reflect.Map:
		old, oldOK := bsonDocument(previous)
		next, nextOK := bsonDocument(replacement)
		if !oldOK || !nextOK {
			return replacement, nil
		}
		if typ.Kind() == reflect.Struct {
			fields := bsonStructFields(typ)
			for key, value := range old {
				if _, known := fields[key]; !known {
					if _, supplied := next[key]; !supplied {
						next[key] = value
					}
				}
			}
			for key, value := range next {
				fieldType, known := fields[key]
				oldValue, existed := old[key]
				if known && existed {
					merged, err := preserveUnknownBSON(oldValue, value, fieldType)
					if err != nil {
						return nil, fmt.Errorf("field %s: %w", key, err)
					}
					next[key] = merged
				}
			}
		} else {
			for key, value := range next {
				if oldValue, existed := old[key]; existed {
					merged, err := preserveUnknownBSON(oldValue, value, typ.Elem())
					if err != nil {
						return nil, fmt.Errorf("map entry %s: %w", key, err)
					}
					next[key] = merged
				}
			}
		}
		return next, nil
	case reflect.Slice, reflect.Array:
		old, oldOK := previous.(bson.A)
		next, nextOK := replacement.(bson.A)
		if !oldOK || !nextOK {
			return replacement, nil // binary/scalar values are not documents
		}
		elem := typ.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct && elem.Kind() != reflect.Map {
			return replacement, nil
		}
		// Match retained records by their complete known value, not by index:
		// reordering/deleting edges must not move a future edge field onto a
		// different edge. Changed records are replacements; their opaque
		// extension's meaning cannot safely be inferred by this older writer.
		knownOld := make([]any, len(old))
		used := make([]bool, len(old))
		for i, value := range old {
			var err error
			knownOld[i], err = knownBSONValue(value, elem)
			if err != nil {
				return nil, err
			}
		}
		for i, value := range next {
			known, err := knownBSONValue(value, elem)
			if err != nil {
				return nil, err
			}
			// The common rename/save keeps order: avoid a quadratic scan
			// over a long unchanged list just to skip already-used entries.
			match := -1
			if i < len(old) && !used[i] && reflect.DeepEqual(knownOld[i], known) {
				match = i
			}
			for j, candidate := range knownOld {
				if match >= 0 {
					break
				}
				if used[j] || !reflect.DeepEqual(candidate, known) {
					continue
				}
				match = j
			}
			if match >= 0 {
				used[match] = true
				next[i], err = preserveUnknownBSON(old[match], value, elem)
				if err != nil {
					return nil, err
				}
			}
		}
		return next, nil
	}
	return replacement, nil
}

// knownBSONValue normalizes a record through this binary's actual codec,
// including omitted/zero fields, before comparing collection identities.
func knownBSONValue(value any, typ reflect.Type) (any, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := bson.Marshal(value)
	if err != nil {
		return nil, err
	}
	known := reflect.New(typ).Interface()
	if err := bson.Unmarshal(raw, known); err != nil {
		return nil, err
	}
	raw, err = bson.Marshal(known)
	if err != nil {
		return nil, err
	}
	decoder := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	decoder.DefaultDocumentM()
	var normalized bson.M
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}
