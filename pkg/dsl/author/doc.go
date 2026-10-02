// Package author reads and writes the YAML twin of a `.bot` file — the
// author document of lot 5 of #1010: the same declarations as the `.bot`,
// in the `.bot`'s order, with YAML's native values, whose shape the author
// JSON Schema (pkg/dsl/spec, docs/references/iterion-author.schema.json)
// describes. The document is a way of WRITING a `.bot`, never a second
// truth: nothing discovers or launches one, `validate` reads it and
// `fmt --to bot` converts it.
//
// Parse does not interpret the document a second time. It SPELLS the YAML
// tree into `.bot` text, one line per YAML construct, each value written in
// the form the registry (pkg/dsl/spec) gives its property — a string
// quoted with the standard escapes, a name bare, an integer as digits, a
// list inline, a block indented — and hands that text to the one parser
// the language has. What the `.bot` accepts, refuses, defaults or names in
// a diagnostic is therefore decided once, by the parser; the converter
// decides only what YAML can say that the text cannot (a negative number,
// an alias, a float where an integer is wanted) and refuses that by name.
// Every position of the AST and of every diagnostic is then mapped back
// onto the YAML node the line came from, so an author reads their own file
// in the report.
//
// Write is the inverse: the AST rendered as a YAML document, keys in the
// registry's order, nodes in the order the `.bot` declared them, edges as
// the `.bot`'s own edge lines. Parse(Write(f)) is the program f, and
// Write(Parse(Write(f))) is Write(f) — proven on every `.bot` of the
// repository (roundtrip_test.go).
package author
