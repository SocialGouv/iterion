package ir

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
)

// ResolveVarTextExact reads a var through the native expansion and coercion
// pipeline, retaining JSON number lexemes. The caller must explicitly select
// this reader from the run's format; the input's shape never selects a format.
// Non-JSON text and comma-separated lists keep their existing interpretation.
// Text declared as float selects finite IEEE. Native inputs use the common
// materialization contract: built-in numeric domains are preserved; named types,
// pointers and containers normalize exactly as they do after transport. Callers
// still own validation against their declared schema.
func ResolveVarTextExact(ctx context.Context, v any, vt VarType, lookup func(string) string) (any, error) {
	return resolveVarTextExact(ctx, v, vt, varExpanderContext(ctx, vt, lookup, false))
}

// ResolveVarTextExactAsWritten is the exact reading for a view with no run
// environment. Like ResolveVarTextAsWritten, unresolved references stay text.
func ResolveVarTextExactAsWritten(ctx context.Context, v any, vt VarType) (any, error) {
	return resolveVarTextExact(ctx, v, vt, varExpanderContext(ctx, vt, noEnvLookup, true))
}

func resolveVarTextExact(ctx context.Context, v any, vt VarType, expand func(string) (string, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := resolveVarTextWithCoercionContext(ctx, v, vt, expand, func(raw any, typ VarType) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if text, ok := raw.(string); ok {
			// Valid JSON has a stricter fidelity contract than arbitrary text.
			// A duplicate key or invalid Unicode must not fall back to text.
			if typ == VarJSON || typ == VarStringArray && strings.HasPrefix(strings.TrimSpace(text), "[") {
				if json.Valid([]byte(text)) {
					return valuecodec.DecodeJSON(ctx, []byte(text))
				}
			}
		}
		return CoerceVarValue(raw, typ)
	})
	if err != nil {
		return nil, err
	}
	// Expansion can introduce invalid Unicode too. The common materialized
	// value contract materializes the final value without invoking payload methods.
	frame, err := valuecodec.EncodeValue(ctx, out)
	if err != nil {
		return nil, err
	}
	return valuecodec.DecodeValue(ctx, frame)
}
