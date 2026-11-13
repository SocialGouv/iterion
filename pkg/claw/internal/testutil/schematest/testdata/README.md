# Offline MCP schema corpus

These JSON files are actual generator output captured locally on 2026-11-04.
They are deliberately **not normalized**. No provider was called, and the
Playwright capture does not launch a browser. Tests never run these generators
and the validator's embedded meta-schemas require no network access.

| Fixture | Producer and reproduction |
| --- | --- |
| `fastmcp-openapi30.json` | `fastmcp==4.1.6`, `mcp==2.3.0`, Python 3.12; `generate.py` constructs an OpenAPI 3.0.3 document, calls `FastMCP.from_openapi` and captures the in-process MCP client's `list_tools()[0].inputSchema`. |
| `zod-openapi3.json` | `zod@3.25.76`, `zod-to-json-schema@3.25.1`, target `openApi3`; `generate.cjs`. |
| `typebox-tuple.json` | `@sinclair/typebox@0.34.41`, `Type.Object({point: Type.Tuple([Type.Number(), Type.Number()])})`; `generate.cjs`. |
| `google-empty.json` | `github.com/google/jsonschema-go v0.4.2`, `jsonschema.For[struct{}](nil)`; `google.go.txt`. |
| `playwright-navigate.json` | `@playwright/mcp@0.0.68`, resolved `playwright` and `playwright-core` `1.59.0-alpha-1771104257000`; the actual `browser_navigate` tool's `toMcpTool(...).inputSchema`; `generate.cjs`. |

Run each generator in a temporary environment with the exact versions above.
For JavaScript, put `generate.cjs` beside that environment's `node_modules`,
then pass the destination directory. For Python, pass the destination JSON
file. Run the Go program in a separate module and capture stdout. The ordinary
project build does not need any of these generators or their dependencies.

`legacy.json` is a synthetic, independently specified conversion corpus. Each
entry carries the source schema, an expected tree when applicable, and/or
accepted and rejected instances. The Go helper additionally crosses **all
2020-12 keyword shapes** with scalar/container/null values. Each provider test
checks its actual serialized `input_schema` against
`https://json-schema.org/draft/2020-12/schema`, regardless of the source's
`$schema` value. Every emitted schema is also compiled, even without instances;
the loader rejects any resource not embedded or explicitly supplied by the case.
Valid URI strings in the shape sweep point to local definitions or the standard
meta-schema. The custom dialect cases register their fixture meta-schema.

`r3-cases.json` contains the round-3 reference, collision, dialect and open-tail
witnesses, including all 12 formerly surviving mutations (MA, MB, MD, ME, MF,
MG, MK, ML, MP, MW, MX, MY). C1/C1b/C2/C2b/C4 deliberately have no instance
witnesses: compilation alone must detect their formerly dangling references.
The dependencies-collision case also rejects instances violating only the modern
half. OpenAI tests separately capture chat and Responses bodies for tuple tails
and property ordering; no provider is contacted.

`r4-cases.json` adds closed tuples with `unevaluatedItems` at the same position,
through `$ref` and `allOf`, and in declared 2019-09 tuples. Each rejects
`["a",1]`. It also checks that a draft-06/07 `$id` with an unrepresentable
anchor retains its base scope, using different types at the inner and outer
pointer targets; the draft-04 `id` case is the control. Round-4 ordered-wire
witnesses in `internal/api/schema_r3_test.go` cover last-value duplicate keys,
unique output keys, deterministic synthesized keys and dependency collisions,
opaque `nullable` under declared dialects, and `id` under 2019-09.

Sources:

- [FastMCP producer report and reproduction](https://github.com/PrefectHQ/fastmcp/issues/5682)
- [zod-to-json-schema targets](https://github.com/StefanTerdell/zod-to-json-schema)
- [TypeBox](https://github.com/sinclairzx81/typebox)
- [google/jsonschema-go](https://github.com/google/jsonschema-go)
- [Playwright MCP](https://github.com/microsoft/playwright-mcp)
