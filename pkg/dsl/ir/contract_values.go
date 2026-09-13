package ir

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"slices"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// DecodePortValue preserves integer precision and rejects trailing values.
// It is shared by defaults and the native JSON input/publication boundaries.
func DecodePortValue(raw []byte) (any, error) {
	return spec.DecodePublicJSON(raw)
}

func nullPortValue(value any) bool {
	if value == nil {
		return true
	}
	r := reflect.ValueOf(value)
	switch r.Kind() {
	case reflect.Map, reflect.Slice, reflect.Pointer, reflect.Interface:
		return r.IsNil()
	default:
		return false
	}
}

// ValidateValue checks data shape only. File publication additionally requires
// the runtime's filesystem/provenance checks; a valid descriptor is not proof
// that a file exists or belongs to the current invocation.
func (p PublicPort) ValidateValue(value any) error {
	if nullPortValue(value) {
		if p.Nullable {
			return nil
		}
		return fmt.Errorf("port %q does not accept null", p.Name)
	}
	if err := p.Type.ValidateValue(value); err != nil {
		return fmt.Errorf("port %q: %w", p.Name, err)
	}
	if p.Type.ArrayDepth > 0 {
		n := reflect.ValueOf(value).Len()
		if p.MinItems != nil && n < *p.MinItems {
			return fmt.Errorf("port %q needs at least %d items, got %d", p.Name, *p.MinItems, n)
		}
		if p.MaxItems != nil && n > *p.MaxItems {
			return fmt.Errorf("port %q allows at most %d items, got %d", p.Name, *p.MaxItems, n)
		}
	}
	return nil
}

func (t PortType) ValidateValue(value any) error {
	if t.ShapeHash == "" || t.ArrayDepth < 0 {
		return fmt.Errorf("unresolved or malformed public type")
	}
	if nullPortValue(value) {
		if t.ArrayDepth == 0 && t.Name == "json" {
			return nil
		}
		return fmt.Errorf("expected %s, got null", t.String())
	}
	if element, array := t.Element(); array {
		r := reflect.ValueOf(value)
		if r.Kind() != reflect.Array && r.Kind() != reflect.Slice {
			return fmt.Errorf("expected %s, got %T", t.String(), value)
		}
		for i := 0; i < r.Len(); i++ {
			if err := element.ValidateValue(r.Index(i).Interface()); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
		return nil
	}
	if t.Schema != nil {
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object for %s, got %T", t.Name, value)
		}
		for _, field := range t.Schema.Fields {
			v, present := object[field.Name]
			if !present {
				return fmt.Errorf("schema %s requires field %q", t.Name, field.Name)
			}
			if nullPortValue(v) {
				return fmt.Errorf("schema %s field %q is null", t.Name, field.Name)
			}
			fieldType, err := ResolvePortType(field.Type.String(), nil)
			if err != nil {
				return err
			}
			if err := fieldType.ValidateValue(v); err != nil {
				return fmt.Errorf("field %q: %w", field.Name, err)
			}
			if len(field.EnumValues) != 0 {
				if field.Type == FieldTypeStringArray {
					r := reflect.ValueOf(v)
					for i := 0; i < r.Len(); i++ {
						if !slices.Contains(field.EnumValues, fmt.Sprint(r.Index(i).Interface())) {
							return fmt.Errorf("field %q item %d violates its enum", field.Name, i)
						}
					}
				} else if !slices.Contains(field.EnumValues, fmt.Sprint(v)) {
					return fmt.Errorf("field %q violates its enum", field.Name)
				}
			}
		}
		return nil
	}
	valid := false
	switch t.Name {
	case "string":
		_, valid = value.(string)
	case "bool":
		_, valid = value.(bool)
	case "int":
		valid = validPortNumber(value, true)
	case "float":
		valid = validPortNumber(value, false)
	case "json":
		_, err := json.Marshal(value)
		valid = err == nil
	case "file":
		// A descriptor is data. Resolve it into a verified artifact before
		// publishing; arbitrary model-supplied metadata is not trusted.
		switch v := value.(type) {
		case string:
			valid = v != ""
		case map[string]any:
			path, ok := v["path"].(string)
			valid = ok && path != ""
		}
	}
	if !valid {
		return fmt.Errorf("expected %s, got %T", t.String(), value)
	}
	return nil
}

func validPortNumber(value any, integer bool) bool {
	switch n := value.(type) {
	case json.Number:
		if !json.Valid([]byte(n)) {
			return false
		}
		r, ok := new(big.Rat).SetString(string(n))
		return ok && (!integer || r.IsInt())
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return validPortNumber(float64(n), integer)
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return false
		}
		return !integer || (math.Trunc(n) == n && math.Abs(n) <= 1<<53)
	default:
		return false
	}
}

// ValidatePublicValues validates declared ports while leaving executor-private
// metadata alone. Defaults are applied by the caller only for unconnected
// inputs; a connected producer returning no value cannot be hidden by a default.
func ValidatePublicValues(ports []PublicPort, values map[string]any) error {
	for _, p := range ports {
		value, present := values[p.Name]
		if !present {
			if p.Required {
				return fmt.Errorf("required port %q is absent", p.Name)
			}
			continue
		}
		if err := p.ValidateValue(value); err != nil {
			return err
		}
	}
	return nil
}

func (c *PublicContract) ValidateCriteria(direction string, values map[string]any) error {
	for _, criterion := range c.Criteria {
		endpoint, err := ParsePortEndpoint(criterion.Port)
		if err != nil {
			return err
		}
		if endpoint.Node != direction {
			continue
		}
		value, present := values[endpoint.Port]
		if !present || nullPortValue(value) {
			// Requiredness and nullability are checked before criteria.
			continue
		}
		check, err := spec.CompilePublicCriterion(criterion.Kind, criterion.Params)
		if err != nil {
			return err
		}
		if err := check(value); err != nil {
			return fmt.Errorf("criterion %q on %s: %w", criterion.Name, criterion.Port, err)
		}
	}
	return nil
}
