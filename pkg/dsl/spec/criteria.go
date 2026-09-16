package spec

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"
)

// CriterionParameter is a machine-readable parameter of a deterministic
// acceptance check. The same declaration serves compilation and authoring.
type CriterionParameter struct {
	Name     string `json:"name"`
	Type     Form   `json:"type"`
	Required bool   `json:"required"`
}

// PublicCriterionSpec is one registered deterministic check: its parameter
// declaration and its evaluator. Neither a scheduler nor an LLM interprets
// descriptive text — a criterion is data and a function.
type PublicCriterionSpec struct {
	Name        string                                        `json:"name"`
	Description string                                        `json:"description"`
	Types       []string                                      `json:"types"`
	Parameters  []CriterionParameter                          `json:"parameters"`
	Compile     func(map[string]any) (func(any) error, error) `json:"-"`
}

// PublicCriteria is the set of evaluators the engine ships. The declaration
// surface is open: a contract may name a kind this table does not have — it
// is declared, rendered, and not evaluated (C303) — so a plugin-supplied
// evaluator is the seam, not a registry edit.
var PublicCriteria = []PublicCriterionSpec{
	{
		Name: "min_length", Description: "Minimum Unicode character or array element count",
		Types:      []string{"string", "array"},
		Parameters: []CriterionParameter{{Name: "min", Type: Int, Required: true}},
		Compile: func(params map[string]any) (func(any) error, error) {
			raw, ok := params["min"].(json.Number)
			if !ok {
				return nil, fmt.Errorf("min must be a non-negative integer")
			}
			n, err := strconv.ParseInt(string(raw), 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("min must be a non-negative integer")
			}
			return func(value any) error {
				var length int
				switch v := value.(type) {
				case string:
					length = utf8.RuneCountInString(v)
				default:
					rv := reflect.ValueOf(value)
					if !rv.IsValid() || (rv.Kind() != reflect.Array && rv.Kind() != reflect.Slice) {
						return fmt.Errorf("min_length requires a string or array")
					}
					length = rv.Len()
				}
				if int64(length) < n {
					return fmt.Errorf("length %d is below required minimum %d", length, n)
				}
				return nil
			}, nil
		},
	},
	{
		Name: "pattern", Description: "String matches a Go regular expression",
		Types:      []string{"string"},
		Parameters: []CriterionParameter{{Name: "pattern", Type: String, Required: true}},
		Compile: func(params map[string]any) (func(any) error, error) {
			source, ok := params["pattern"].(string)
			if !ok {
				return nil, fmt.Errorf("pattern must be a string")
			}
			re, err := regexp.Compile(source)
			if err != nil {
				return nil, fmt.Errorf("invalid pattern: %w", err)
			}
			return func(value any) error {
				v, ok := value.(string)
				if !ok || !re.MatchString(v) {
					return fmt.Errorf("value does not match pattern %q", re.String())
				}
				return nil
			}, nil
		},
	},
}

// LookupPublicCriterion finds a registered evaluator by name.
func LookupPublicCriterion(name string) (PublicCriterionSpec, bool) {
	for _, c := range PublicCriteria {
		if c.Name == name {
			return c, true
		}
	}
	return PublicCriterionSpec{}, false
}

// CompilePublicCriterion validates raw parameters against the criterion's
// declaration — every required parameter present, every present one of its
// declared type, none undeclared, keys unique — and returns its evaluator.
func CompilePublicCriterion(name string, raw json.RawMessage) (func(any) error, error) {
	c, ok := LookupPublicCriterion(name)
	if !ok {
		return nil, fmt.Errorf("unknown public criterion %q", name)
	}
	params := map[string]any{}
	if len(raw) != 0 {
		value, err := DecodePublicJSON(raw)
		if err != nil {
			return nil, err
		}
		var object bool
		params, object = value.(map[string]any)
		if !object {
			return nil, fmt.Errorf("criterion %q parameters must be a JSON object", name)
		}
	}
	declared := map[string]bool{}
	for _, p := range c.Parameters {
		declared[p.Name] = true
		value, present := params[p.Name]
		if !present {
			if p.Required {
				return nil, fmt.Errorf("criterion %q requires parameter %q", name, p.Name)
			}
			continue
		}
		switch p.Type {
		case String:
			if _, ok := value.(string); !ok {
				return nil, fmt.Errorf("parameter %q must be a string", p.Name)
			}
		case Int:
			n, ok := value.(json.Number)
			if !ok {
				return nil, fmt.Errorf("parameter %q must be an integer", p.Name)
			}
			if _, err := n.Int64(); err != nil {
				return nil, fmt.Errorf("parameter %q must be an integer", p.Name)
			}
		default:
			return nil, fmt.Errorf("criterion %q declares unsupported parameter type %q", name, p.Type)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(params)) {
		if !declared[key] {
			return nil, fmt.Errorf("criterion %q has no parameter %q", name, key)
		}
	}
	return c.Compile(params)
}
