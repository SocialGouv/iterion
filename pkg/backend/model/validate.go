package model

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// isMissingFieldError reports whether err (produced by ValidateOutput)
// carries at least one "missing required field" cause. The check is
// substring-based — ValidateOutput joins multiple cause strings with
// "; " and we want to retry whenever any cause is a missing-field
// (even if a type error is mixed in). See validateAndRetry for why
// missing-field is treated as retry-eligible while type/enum errors
// are not.
func isMissingFieldError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "missing required field")
}

// ValidateOutput checks that output contains all required fields from the
// schema with compatible types. It does NOT attempt to repair or coerce
// invalid values — the node must fail explicitly on schema mismatch.
func ValidateOutput(output map[string]any, schema *ir.Schema) error {
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
func checkFieldType(f *ir.SchemaField, val any) error {
	if val == nil {
		return fmt.Errorf("field %q is null", f.Name)
	}

	switch f.Type {
	case ir.FieldTypeString:
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("field %q: expected string, got %T", f.Name, val)
		}
		if len(f.EnumValues) > 0 && !slices.Contains(f.EnumValues, s) {
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

	case ir.FieldTypeFile:
		// Accept both shapes the field can legitimately carry: the
		// descriptor map the resume path builds after promoting the
		// upload, and a bare path string (an LLM-answered gate, or a
		// CLI --answer naming a file already on disk). Anything else is
		// a genuine mismatch.
		switch v := val.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("field %q: file path is empty", f.Name)
			}
		case map[string]any:
			p, _ := v["path"].(string)
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("field %q: file descriptor has no path", f.Name)
			}
		default:
			return fmt.Errorf("field %q: expected file descriptor or path, got %T", f.Name, val)
		}

	case ir.FieldTypeStringArray:
		arr, ok := val.([]any)
		if !ok {
			return fmt.Errorf("field %q: expected string array, got %T", f.Name, val)
		}
		for i, item := range arr {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("field %q[%d]: expected string, got %T", f.Name, i, item)
			}
			// String arrays inherit the field's enum constraint
			// (FieldTypeString applies it per scalar — without this
			// check the schema-level enum was advertised to the LLM
			// but never enforced server-side, so a stray value would
			// flow downstream unchecked).
			if len(f.EnumValues) > 0 && !slices.Contains(f.EnumValues, s) {
				return fmt.Errorf("field %q[%d]: value %q not in enum %v", f.Name, i, s, f.EnumValues)
			}
		}
	}

	return nil
}
