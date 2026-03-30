package model

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/ir"
)

// ValidateOutput checks that output contains all required fields from the
// schema with compatible types. It does NOT attempt to repair or coerce
// invalid values — the node must fail explicitly on schema mismatch.
func ValidateOutput(output map[string]interface{}, schema *ir.Schema) error {
	var errs []string

	for _, f := range schema.Fields {
		val, ok := output[f.Name]
		if !ok {
			errs = append(errs, fmt.Sprintf("missing required field %q", f.Name))
			continue
		}

		if err := checkFieldType(f, val); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// checkFieldType validates that val is compatible with the expected field type.
func checkFieldType(f *ir.SchemaField, val interface{}) error {
	if val == nil {
		return fmt.Errorf("field %q is null", f.Name)
	}

	switch f.Type {
	case ir.FieldTypeString:
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("field %q: expected string, got %T", f.Name, val)
		}
		if len(f.EnumValues) > 0 && !contains(f.EnumValues, s) {
			return fmt.Errorf("field %q: value %q not in enum %v", f.Name, s, f.EnumValues)
		}

	case ir.FieldTypeBool:
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("field %q: expected bool, got %T", f.Name, val)
		}

	case ir.FieldTypeInt:
		// JSON numbers deserialize as float64; accept whole numbers.
		switch v := val.(type) {
		case float64:
			if v != float64(int64(v)) {
				return fmt.Errorf("field %q: expected integer, got float %v", f.Name, v)
			}
		default:
			return fmt.Errorf("field %q: expected integer, got %T", f.Name, val)
		}

	case ir.FieldTypeFloat:
		if _, ok := val.(float64); !ok {
			return fmt.Errorf("field %q: expected number, got %T", f.Name, val)
		}

	case ir.FieldTypeJSON:
		// Any non-nil value is acceptable for JSON fields.

	case ir.FieldTypeStringArray:
		arr, ok := val.([]interface{})
		if !ok {
			return fmt.Errorf("field %q: expected string array, got %T", f.Name, val)
		}
		for i, item := range arr {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("field %q[%d]: expected string, got %T", f.Name, i, item)
			}
		}
	}

	return nil
}

func contains(vals []string, s string) bool {
	for _, v := range vals {
		if v == s {
			return true
		}
	}
	return false
}
