# ADR-083 — JSON-typed schema fields compile to a type union, not the empty schema

- Status: accepted
- Date: 2026-07-22
- Deciders: devthejo

## Context

The DSL `json` field type (`ir.FieldTypeJSON`) means "this output field
accepts any JSON value" — object, array, string, number, boolean, or
null. `SchemaToJSON` ([pkg/backend/model/schema.go](../../pkg/backend/model/schema.go))
compiles each IR schema into the JSON Schema every delegate hands to its
provider as the structured-output contract (`task.OutputSchema`, consumed
by claude_code, claw, and codex alike — there is a single conversion
point).

The canonical JSON Schema for "any value" is the **empty schema `{}`**
(no `type` key). Two live production bugs, pulling in opposite
directions, proved that neither `{}` nor a single concrete `type` is a
safe compilation for a strict structured-output pass:

1. **A single `"type": "object"` rejects every non-object shape.** Seen
   on secured-renovacy/main.bot (run_1778786106222 sonnet+high,
   run_1778784391171 opus+max): `detect_stack` populated a recipe's
   `ecosystems: json` field as a JSON **array** (the only sensible shape
   for "list of per-ecosystem profiles"). The derived schema declared
   `{"type": "object"}`, JSON Schema rejected the array, the formatter
   stripped the value to nothing → `raw_output_len: 0` + "missing
   required field ecosystems".

2. **The empty schema `{}` (no `type` key) is rejected outright by
   OpenAI/codex's structured-output formatting pass** with
   `invalid_json_schema: In context=('properties', <field>), schema must
   have a 'type' key` — 400ing the whole node, surfacing as
   `delegate: codex formatting pass returned empty structured output` →
   `failed_resumable`, even when the agent's own final message carried a
   valid payload. Anthropic tolerates `{}`; OpenAI does not.

`iterion validate` accepted the schema silently in both cases — the
failure only appeared at the first real execution against the provider.

## Decision

Compile a `FieldTypeJSON` property to a **`type` union over every JSON
kind**:

```json
{ "type": ["object", "array", "string", "number", "boolean", "null"] }
```

This satisfies both constraints simultaneously: the `type` key is
present (bug 2 — providers that require one accept it), and every JSON
shape remains valid (bug 1 — arrays/scalars/null are not stripped). It
is the faithful wire encoding of `FieldTypeJSON`'s "accepts any value"
contract for providers that reject the type-less `{}`.

### Alternative rejected — flag/reject `json` fields at validate time

The feature admitted a second reading: reject or warn on `json`-typed
fields in LLM-node output schemas at compile time ("json fields are not
representable in strict structured output — use a typed field"). We did
**not** take this path:

- `json` is a legitimate, shipped field type used by real recipes
  (`ecosystems`, free-form `questions` payloads) where the value
  genuinely has no fixed shape. Rejecting it would force authors into
  lossy `string[]` workarounds for data that is not a string array.
- The union makes the schema **valid**, so the validate-time silent
  acceptance is now correct behaviour, not a latent trap — there is
  nothing left to flag.

A provider-specific branch (emit `{}` for Anthropic, the union for
OpenAI) was also rejected: the union is valid for both, so one encoding
keeps `SchemaToJSON` provider-agnostic — the same compiled contract
behaves identically on either backend, matching the "a bot behaves
identically on either backend" invariant the rest of the stack upholds.

## Consequences

- A `json` output field is portable across all three delegates with no
  per-provider handling.
- The compiled property is permissive (any JSON kind) — this is
  intentional; `FieldTypeJSON` is the author's explicit opt-out of shape
  constraints. Authors who want a shape use a typed field or a nested
  schema.
- Pinned by `TestSchemaToJSON_JSONFieldIsAnyType`
  ([pkg/backend/model/schema_test.go](../../pkg/backend/model/schema_test.go)),
  which asserts both directions: the `type` key is present (bug 2) and
  the union admits every JSON kind including `array` (bug 1). Re-narrowing
  to a single type or dropping the key will fail the test.
