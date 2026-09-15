# ADR-099: Public contracts, bound to the program

**Status:** Accepted (2026-09-15, lot 4bis of #1010; harvests #1216 / #1165)
**Amends:** ADR-098 § 16 (the reserved `runtime_semantics` axis)

## Context

A bot has a face the outside reads without opening its prompts: what it takes,
what it produces, the files it delivers, the checks that condition what
follows, and the effects it has. Today that face is implicit — a parent's
`subbot` guesses it from the child's `vars` and its last node's schema, the
catalogue reads a manifest that repeats it by hand, and the snapshot carries
nothing of it. PR #1216 proposed a `contract` declaration together with a
`graph` of data-flow across nodes, a `port_policy`, and a `runtime_semantics`
axis to run them under.

ADR-098 § 16 reserved the `runtime_semantics` axis for that work. Reviewing
#1216 against the program found the surface worth keeping and the semantics
premature: nothing in the engine runs a data-flow graph, so a `graph:` would
be text the runtime ignores — a contract the program does not keep, displayed
as if it did.

The decisive point of the adversarial plan review (31 findings, 0 refuted):
**a contract that is not checked against the program it describes is worse
than none**, because every reader — the catalogue, the parent bot, the
studio — trusts it.

## Decision

1. **One top-level declaration, `contract <name>:`, named by the workflow.**
   `workflow x:` gains `contract: <name>` (the property is appended last in
   the registry so no existing position moves). A contract carries
   `display_name`, `responsibility`, `version`, `inputs`, `outputs`,
   `criteria`, `effects` — and no prompt, tool or provider setting. A bare
   header declares an empty contract, like every other named declaration.

2. **The contract is bound to the program.** The compiler holds it:
   - **C300** — every input is a declared var of the same name, required
     exactly when the var has no default and defaulting to what the var
     defaults to — a port may repeat the var's word, never contradict it;
     an input is refused a `from:`.
   - **C301** — every output names its producer, `from: <node>.<field>`, and
     the field's declared type matches the port's type structurally (a
     builtin, a schema name, `[]` cardinality); a file port names its node,
     `from: <node>`.
   - **C302** — a criterion names a port that exists (`input.x` /
     `output.y`), its `params` compile against the criterion's parameter
     declaration, and every `default:` and `params:` value is one the text
     can write back (see 4).
   - **C303** (warning) — a criterion whose `kind` no shipped evaluator
     bears is declared and rendered, not evaluated.

3. **No `graph`, no `port_policy`, no `runtime_semantics` property.** The
   axis stays reserved (ADR-098 § 16, amended): its property ships with the
   semantics that runs under it, and a `contract:` is legal without it. A
   future data-flow semantics composes contracts across nodes; it never
   redefines what a contract is.

4. **JSON values are the writable subset.** A port's `default:` and a
   criterion's `params:` are JSON, read by the parser and written by the
   unparser. The text's lexer reads a number as `[0-9]+(\.[0-9]+)?` — no
   sign, no exponent — so a value the transport accepts (the studio's JSON
   carries any number) is refused at C302 by `parser.WritableJSONValue`
   before the writer ever meets it. Object keys are unique
   (`spec.DecodePublicJSON` refuses a duplicate), `null` is explicit and
   distinct from absence.

5. **Criteria are data and a function.** `spec.PublicCriteria` ships the
   evaluators (`min_length`, `pattern`) with a machine-readable parameter
   declaration each; `CompilePublicCriterion` validates the parameters
   (required present, types, no undeclared key) and returns the evaluator.
   The evaluator set is closed; the declaration surface is open (C303), so a
   plugin-supplied evaluator is the seam, not a registry edit.

6. **The contract travels like every declaration.** The JSON mirror
   (`ast/jsonenc.go`) carries `contracts[]` and the workflow's `contract`,
   the unparser writes them back, and the AST-level round-trip guard holds
   text → AST → JSON → AST → text on a witness that uses every property. The
   port's `file:` block is the field `FileSpec` / key `file_spec`, because
   `file` is the provenance key every mirror already carries.

7. **A floor, derived.** `ContractSince` is the engine version the contract
   surface ships in; the sites that decide "does this build read this text"
   derive from the single `RequiredRelease` predicate, so a bundle whose
   `requires.iterion` is older than the floor and that declares a contract is
   refused by C251, and by nothing else.

8. **What a contract is not.** `effects:` document; they grant nothing — the
   sandbox, the allow-lists and the verified actions keep the admission.
   File existence, provenance and immutable publication are the runtime's
   checks, never an author's assertion. A contract does not replace the
   manifest; the catalogue reads the contract when one exists.

## Consequences

- The `.bot` gains one declaration kind and one workflow property; no
  existing text changes meaning (v1 and v2 alike — the profile does not
  govern it, the floor bounds it).
- A contract that lies about its program is a compile error, not a display.
- The dogfood — `feature-dev` declaring its contract — lands in a follow-up
  PR after the release that carries the floor and the fleet bump, never in
  the PR that ships the surface (a bot the fleet cannot read is a broken
  bot, not a demo).
- Two bundlelint warnings follow (C254/C255): a manifest that repeats what
  the contract says, and a `subbot` whose `with:` misses a required input
  of the child's contract.

## Alternatives rejected

- **Shipping `graph` and `port_policy` as text only** (#1216 as proposed):
  a declared semantics nothing runs; refused on the "no artificial
  limitation, no silent promise" stance.
- **A contract inferred from the program** (vars + last node's schema): no
  place for `responsibility`, `effects`, files or criteria, and the parent
  still guesses which node's schema is the output.
- **A contract kept in the manifest** (`manifest.yaml`): outside the
  compilation unit, unchecked by the compiler, invisible to the unparser.
