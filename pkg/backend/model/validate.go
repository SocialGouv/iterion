package model

import (
	"fmt"
	"math"
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

// ConformComputeOutput conforms a compute node's evaluated output to its
// declared schema, IN PLACE. It is the compute-side counterpart of
// ValidateOutput: an LLM's output arrives JSON-shaped (every number a
// float64), an expression's output arrives Go-shaped (an integer division
// yields an int64, a float one a float64), and a declared type has to mean
// the same thing on both.
//
// Numeric representation is normalised to the declared type — an integral
// float under `int` becomes an int64, an integer under `float` a float64 —
// and a `string[]` built as a []string becomes the []any the rest of the
// engine reads. Everything else is a mismatch and fails with the field
// named, under the same rule checkFieldType applies to an LLM's output; a
// fractional float under `int` additionally names the builtins that make
// the rounding explicit, because the engine will not pick one.
//
// A field the expression left nil, or that no expression produced, is left
// alone: that is the absence class, not a type mismatch.
func ConformComputeOutput(output map[string]any, schema *ir.Schema) error {
	if schema == nil {
		return nil
	}
	var errs []string
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		val, ok := output[f.Name]
		if !ok || val == nil {
			continue
		}
		conformed, err := conformFieldValue(f, val)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		output[f.Name] = conformed
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// conformFieldValue returns val in the representation its declared type
// reads as, or the mismatch error checkFieldType would report for it.
func conformFieldValue(f *ir.SchemaField, val any) (any, error) {
	switch f.Type {
	case ir.FieldTypeInt:
		n, ok, fractional := integerOf(val)
		if fractional {
			return nil, fmt.Errorf("field %q is declared int but the expression produced %v — make the rounding explicit with floor(...) or round(...)", f.Name, val)
		}
		if !ok {
			return nil, checkFieldType(f, val)
		}
		return n, nil
	case ir.FieldTypeFloat:
		x, ok := numberOf(val)
		if !ok {
			return nil, checkFieldType(f, val)
		}
		return x, nil
	case ir.FieldTypeStringArray:
		if ss, ok := val.([]string); ok {
			arr := make([]any, len(ss))
			for i, s := range ss {
				arr[i] = s
			}
			val = arr
		}
	}
	return val, checkFieldType(f, val)
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
		// A JSON number deserialises as a float64, an expression evaluates
		// to an int64: both are integers when whole.
		_, ok, fractional := integerOf(val)
		if fractional {
			return fmt.Errorf("field %q: expected integer, got float %v", f.Name, val)
		}
		if !ok {
			return fmt.Errorf("field %q: expected integer, got %T", f.Name, val)
		}

	case ir.FieldTypeFloat:
		if _, ok := numberOf(val); !ok {
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

// integerOf reports val as an int64 when it is an integer under any Go
// numeric representation: a native integer kind, or a float with no
// fractional part (the JSON decoder's shape for every number). fractional
// is true when val is a float that has one — the one mismatch a caller
// words differently from "wrong type", since the fix is a rounding, not a
// retype.
func integerOf(val any) (n int64, ok bool, fractional bool) {
	switch v := val.(type) {
	case int:
		return int64(v), true, false
	case int8:
		return int64(v), true, false
	case int16:
		return int64(v), true, false
	case int32:
		return int64(v), true, false
	case int64:
		return v, true, false
	case uint8:
		return int64(v), true, false
	case uint16:
		return int64(v), true, false
	case uint32:
		return int64(v), true, false
	case uint:
		if uint64(v) > math.MaxInt64 {
			return 0, false, false
		}
		return int64(v), true, false
	case uint64:
		if v > math.MaxInt64 {
			return 0, false, false
		}
		return int64(v), true, false
	case float32:
		return integerOfFloat(float64(v))
	case float64:
		return integerOfFloat(v)
	}
	return 0, false, false
}

func integerOfFloat(v float64) (int64, bool, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < math.MinInt64 || v >= math.MaxInt64 {
		return 0, false, false
	}
	if v != math.Trunc(v) {
		return 0, false, true
	}
	return int64(v), true, false
}

// numberOf reports val as a float64 when it is a number of any Go kind.
func numberOf(val any) (float64, bool) {
	switch v := val.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	}
	if n, ok, _ := integerOf(val); ok {
		return float64(n), true
	}
	return 0, false
}
