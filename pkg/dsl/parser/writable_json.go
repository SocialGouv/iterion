package parser

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// writableNumber is the number the lexer reads: digits, an optional
// fraction — no sign, no exponent (scanNumber).
var writableNumber = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// WritableJSONValue reports whether raw — a contract port's `default:` or a
// criterion's `params:` — is a JSON value the .bot text can write back as
// it is: one value, object keys unique, and every number without a sign or
// an exponent. The text has neither, so a `-1` or a `1e-3` accepted from a
// document (the studio's transport carries any JSON) could never be saved:
// the writer would emit a text that does not parse, and the save would be
// refused without naming the cause. The compiler refuses such a value where
// it is declared (C302), so the writer never meets one.
func WritableJSONValue(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	value, err := spec.DecodePublicJSON(raw)
	if err != nil {
		return err
	}
	return writableJSON(value, "")
}

func writableJSON(value any, path string) error {
	switch v := value.(type) {
	case json.Number:
		if !writableNumber.MatchString(string(v)) {
			return fmt.Errorf("the number %s%s cannot be written in a .bot, which has no signed number and no exponent: write a positive decimal, or a string", v, at(path))
		}
	case []any:
		for i, e := range v {
			if err := writableJSON(e, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := writableJSON(v[k], path+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}

func at(path string) string {
	if path == "" {
		return ""
	}
	return " at " + path
}
