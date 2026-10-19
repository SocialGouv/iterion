package claudesdk

// strictStructuredOutput rewrites the JSON Schema parts the claude-code
// CLI's structured-output validator only tolerates as warnings — the DSL's
// `json` output fields materialize (model.SchemaToJSON) as a `type` union
// over every JSON kind (the shape codex/OpenAI require, because they reject
// a type-less property), and the validator prints
// `use allowUnionTypes` for it on every spawn. On CLI 2.1.282 that is a
// warning and the structured output still lands — the real failure of
// #2500's run was a provider error captured as the result — but a build
// that resolves strictTypes to `error` would strip the whole structured
// output over it. A property with no `type` at all passes the same
// validator and is the CLI's canonical "any", and is semantically neutral
// for the DSL's any-contract: so every union `type` (the only shape the
// DSL emits) becomes the empty schema here, recursively. The other backends
// keep the union.
func strictStructuredOutput(schema map[string]any) map[string]any {
	strict(schema)
	return schema
}

// strict walks the schema document replacing every union `type` with the
// type-less any-schema, and every `anyOf`/`oneOf` of bare type names with it
// too — the two spellings a union arrives in.
func strict(node any) {
	switch n := node.(type) {
	case map[string]any:
		if types, ok := n["type"].([]any); ok && len(types) > 1 {
			delete(n, "type")
		}
		for _, key := range []string{"anyOf", "oneOf"} {
			if branches, ok := n[key].([]any); ok {
				bare := true
				for _, b := range branches {
					if _, isString := b.(string); !isString {
						bare = false
					}
				}
				if bare && len(branches) > 1 {
					delete(n, key)
				}
			}
		}
		for _, v := range n {
			strict(v)
		}
	case []any:
		for _, v := range n {
			strict(v)
		}
	}
}
