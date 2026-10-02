package tool

import "context"

type builtinAliasesKey struct{}

// WithBuiltinAliases scopes the manifest's engine-floor opt-in to one execution.
// Every child engine writes its own value, including false, so a parent bundle
// cannot grant the new semantics to a bare or older child workflow.
func WithBuiltinAliases(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, builtinAliasesKey{}, enabled)
}

func BuiltinAliasesEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(builtinAliasesKey{}).(bool)
	return enabled
}
