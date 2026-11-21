package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// OutputViolationKind identifies a rule, independently of diagnostic wording.
type OutputViolationKind string

const (
	OutputMissing     OutputViolationKind = "missing"
	OutputNull        OutputViolationKind = "null"
	OutputWrongType   OutputViolationKind = "wrong_type"
	OutputEnum        OutputViolationKind = "enum"
	OutputInvalidFile OutputViolationKind = "invalid_file"
)

// OutputViolation is internal validation evidence. InternalDetail can contain
// model output and must pass the run's scrubber before persistence or logging.
type OutputViolation struct {
	Field          string
	Kind           OutputViolationKind
	InternalDetail string
}

func (v OutputViolation) Error() string { return v.InternalDetail }
func outputViolation(field string, kind OutputViolationKind, format string, args ...any) error {
	return OutputViolation{Field: field, Kind: kind, InternalDetail: fmt.Sprintf(format, args...)}
}

// OutputValidationError renders violations in schema declaration order.
type OutputValidationError struct{ Violations []OutputViolation }

func (e *OutputValidationError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = v.InternalDetail
	}
	return strings.Join(parts, "; ")
}

// CollectOutputViolations shares the existing leaf validator. The explicit
// registry is the composition seam for resolved schemas; this version's IR
// admits only builtin fields, so it does not resolve named fields itself.
func CollectOutputViolations(output map[string]any, schema *ir.Schema, registry map[string]*ir.Schema) []OutputViolation {
	if schema == nil {
		return nil
	}
	var violations []OutputViolation
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		val, ok := output[f.Name]
		if !ok {
			violations = append(violations, OutputViolation{Field: f.Name, Kind: OutputMissing, InternalDetail: fmt.Sprintf("missing required field %q", f.Name)})
			continue
		}
		if err := checkFieldType(f, val); err != nil {
			var violation OutputViolation
			if errors.As(err, &violation) {
				violations = append(violations, violation)
			} else {
				violations = append(violations, OutputViolation{Field: f.Name, Kind: OutputWrongType, InternalDetail: err.Error()})
			}
		}
	}
	return violations
}

// ValidateOutputWithRegistry is the common diagnostic adapter. Callers own
// schema resolution, and pass their workflow registry explicitly.
func ValidateOutputWithRegistry(output map[string]any, schema *ir.Schema, registry map[string]*ir.Schema) error {
	violations := CollectOutputViolations(output, schema, registry)
	if len(violations) == 0 {
		return nil
	}
	return &OutputValidationError{Violations: violations}
}
func isMissingFieldError(err error) bool {
	var validation *OutputValidationError
	if errors.As(err, &validation) {
		for _, v := range validation.Violations {
			if v.Kind == OutputMissing {
				return true
			}
		}
	}
	return false
}

// OutputCorrectionEligible covers structured validation failures. Diagnostic
// text and provider failures never manufacture a schema violation.
func OutputCorrectionEligible(violations []OutputViolation, parseFallback bool) bool {
	return parseFallback || len(violations) > 0
}
