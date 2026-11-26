# ADR-171 — A tool node's declared output is a contract: text where its JSON object belongs fails the node, and a tool body is refused a field its producer owed

- **Status**: Accepted — the operator decided it on 2026-11-20 ("harden, narrow"), and this record ships with the change that implements it (#3421).
- **Date**: 2026-11-20
- **Deciders**: jo (the decision), Claude (the design and the implementation), Claude Opus 5.5 (two adversarial review rounds)
- **Relates to**: [ADR-044](044-adaptive-recovery-for-deterministic-action-nodes.md) (the Verified Action ladder, whose nodes rule A leaves alone) · [ADR-098](098-connector-catalog.md) (the `action:` recipe, outside rule A and a body of rule B) · [ADR-132](132-a-run-reads-no-environment.md) (the engine process's own knobs stay in the environment: the two dials below are such knobs)
- **Serves**: [#3421](https://github.com/SocialGouv/iterion/issues/3421) · neighbours [#3047](https://github.com/SocialGouv/iterion/issues/3047) and [#3057](https://github.com/SocialGouv/iterion/issues/3057) · residues [#3481](https://github.com/SocialGouv/iterion/issues/3481)

## Context

A tool node's stdout is its output: the engine parses it as a JSON object, and that object is what `{{outputs.<node>.<field>}}`, an edge `when` and the node's `output:` schema read. `docs/philosophy.md`, principle 1, sets the engine's default posture on what it cannot verify: warn rather than reject, and a load-bearing limit carries a greppable escape hatch.

Under that posture, a node that declares `output:` and prints something else than a JSON object is not refused. The text is wrapped as `{"result": "<text>"}` (`parseToolNodeOutputWrapped`, `pkg/backend/model/executor_tool.go`), the run raises an advisory `node_output_schema_mismatch` event, and the node finishes green with none of its declared fields. A later tool body that reads `{{outputs.<node>.<field>}}` then meets a hole, which the renderer leaves as the reference's own text in a `command:` — where `bash -c` runs it — and as `null` in a `script:`.

On 2026-11-19 the two leniencies chained on a bot a coding agent had written and `iterion validate` had accepted (#3421). Its `publish` node printed a `git commit` summary ahead of its JSON, so the wrapper was published; the next node ran `curl …/pulls/{{outputs.publish.pr_number}}/merge` against the forge, after a real pull request had been opened. With `curl` as the last command the node failed, and was retried; with `curl …; printf …` the run finished, exit 0 — a green run whose declared fields were all absent. `iterion validate --exec --strict --fixtures` reports the hole; the plain `validate` an author runs does not.

Three answers were on the table: keep the wrapper and only warn; harden narrowly, on the two shapes that chained; harden widely, on every schema mismatch and every hole.

## Decision

The narrow hardening. Two refusals, one dial each, and nothing else moves.

### 1. A declared answer is one JSON object

A `command:` or `script:` that declares `output:` and whose stdout is non-empty and not a JSON object — text beside the object, plain text, an array, a `null`, two objects — fails at that node with `TOOL_FAILED_PERMANENT` (`declaredAnswer` and `ToolStdoutNotJSONError`, `pkg/backend/model/tool_contract.go`). The message names the schema and the number of bytes printed, never the bytes: a tool's stdout is data, and the message is persisted on the run (#3057).

### 2. A tool body is refused a field its tool producer owed

Before a tool body runs, an `{{outputs.<node>.<field>}}` it reads is refused when the producer is a tool node that has produced an output in this run, the producer's own `output:` schema declares the field, and that output does not carry it — absent, or null where the field is not declared `json` (`owedOutputRefs` and `outputRefOwed`, same file). The reading node fails with `TOOL_FAILED_PERMANENT` and a message naming the reference and the producer, never a value. The four bodies of a tool node that render the namespace ask, each where it is about to run: `command:`, `script:` in any language, `postcondition:`, an action's `params:`.

### 3. Both refusals are permanent, wherever they are read

`ClassifyError` reads the typed refusal (`model.ToolContractViolation`) as `TOOL_FAILED_PERMANENT` (`pkg/runtime/error_classify.go`): the recovery recipe fails the run without a retry, the retry policy classes the code deterministic, and the cloud runner acks the delivery instead of redelivering it. A `wait_all` join that one branch failed on such a refusal carries the same code and quotes that branch (`pkg/runtime/convergence.go`). The run parks `failed_resumable`; a resume re-executes the refused node.

### 4. A refusal, where the philosophy prefers a warning

The warning exists — `node_output_schema_mismatch` fired on the run of 2026-11-19 — and the run went on to call the forge. A wrapped output is not a degraded answer a later node can use with care: it is an answer in which every declared field is missing, and the next tool body is a command that executes on that absence. The engine cannot warn its way out of a `curl` that has already been sent.

The philosophy's own condition for a bound holds: each refusal has a greppable override, read once per engine (`pkg/runtime/engine_options.go`).

- `ITERION_STRICT_OUTPUT_VALIDATION` has three positions, and unset is not off: unset, rule 1 and advisory mismatches; `1`, every schema mismatch fails as well; `0` written out, the `{"result": "<text>"}` wrapper is published again.
- `ITERION_STRICT_OUTPUT_REFS=0` restores the rendering of the hole rule 2 refuses.

Both are settings of the engine's process, the operator's: `internal/envtrust/operator.go` lists them, so no bot variable and no project `.env` sets them. The first alone does not bring the earlier run back — the reader of the wrapper is then the node rule 2 refuses.

### 5. What the two rules leave alone

- **A node without `output:`** keeps the text wrapper: it promised no field.
- **Empty stdout under `output:`** keeps the empty wrapper, and **a JSON object missing declared fields** stays reported and kept: both are a producer that said less than it declared, which rule 2 catches at the tool body that reads the missing field, and which #3047 still finds in the catalogue.
- **A node with a `postcondition:`** is outside rule 1: it publishes the merge of its recipe's stdout and its postcondition's and is judged by the latter (ADR-044), so its recipe may print text. **A registry tool** answers with the tool's own string, **an `action:`** with the operation's result; neither is a process's stdout.
- **Only a tool producer owes.** A `human` gate answered in part, a `compute` expression that resolves to an absence and a `subbot` — whose answer is another workflow's — may leave a declared field out by design; an agent and a judge are validated where they answer. Their readers keep the rendering.
- **Every other hole** keeps the rendering: a producer that has not run on this path, a producer without a schema, a field its schema does not declare, every other namespace. gitops-warden relies on a literal `{{input.fallback_used}}` (`TestGitopsWardenFold_UnresolvedFallbackRefIsNotAFallback`).
- **The engine's directive lines** — `[iterion] attachment=`, `preview_url=`, `preview_screenshot=`, the prefixes `scanDirectiveLines` is asked for — are instructions to the engine and are read out of a declared answer before rule 1 judges it (`withoutDirectiveLines`, `pkg/backend/model/preview_url.go`).
- **A join merges colliding branch outputs in the order of their branch ids**, not in the order the branches finished, so what rule 2 reads after a `fan_out_each` join is the same on every run.

## Consequences

- The catalogue nodes that printed beside their answer print their declared object, each in a bundle with a new version: `commit_changes` of adr-rechallenge (0.1.4) and bmady (0.1.5), `mark_issue_for_review` of adr-cartograph (2.1.3), the three state commits of feed-watch (1.7.1); the whats-next quickref (0.3.12) and the root `SKILL.md` teach the contract with an example that passes it. `examples/human-file-upload.bot`, the studio's two e2e seed bots and `e2e/testdata/exhaustive_dsl_coverage.bot` follow.
- A **stored** bot — a team's row, a platform override — that prints beside its JSON under `output:` fails at that node after the upgrade, until its recipe is fixed or the operator writes the dial off. An override of one of the bundles above shows `shadows_newer_version` against the new bake; the operator reads stored overrides before a roll.
- A run paused on an older engine whose producer's output is a wrapper is refused at the tool body that reads it on resume. The cure is the producer's: `iterion rewind --run-id <run> --node <producer>`, then resume.
- A node that was green and wrong fails: an `exit 0` over an HTML error page under `output:`, a missing interpreter that leaves no JSON.
- `internal/sweep/catalogcorpus/catalog_tool_stdout_test.go` reads the shipped recipes for the shapes that have cost a run (a `git commit` child that inherits stdout, a `print` that is not the object, a transfer whose body is the stdout). It is a lint: the refusal at the node is the guard.

## Alternatives rejected

- **Keep the wrapper and only warn** — an event beside the node, or the plain `validate` saying what the dry run knows. It costs no existing bot anything, and it wins wherever a text-printing node's output is never read. It loses on the incident itself: the warning was on the run, and a diagnostic at authoring time reaches neither a stored bot nor a recipe whose stdout depends on what `git` prints that day.
- **Harden widely** — every schema mismatch fails (what `ITERION_STRICT_OUTPUT_VALIDATION=1` gives for that half) and every unresolved reference in every namespace is refused. It wins once nothing shipped depends on a leniency. It loses today on named cases: the catalogue nodes that omit a declared field on a refusal path (`TestR2CatalogToolNodesUnderDefaultValidation`, #3047), gitops-warden's literal `{{input.fallback_used}}`, and every human gate an operator answers in part.

## Assumed limits (stated in `docs/dsl.md`)

- A Verified Action owes its declared fields whichever rung answered: an idempotent skip publishes its postcondition's stdout, and under `policy: best_effort` an unmet postcondition leaves a tool reader refused with no rewind that cures it.
- The directive lines are not read out on a node with a `postcondition:`; there the directive goes to stderr, which the engine scans too.
- A directive is a whole line: a directive and the JSON on one line are read as the directive, and the answer is empty.
- The join's order is the lexicographic order of the branch ids — with twelve items `…_9` sorts after `…_11` — so a bot does not rely on a collided output after a join.
- A reference inside a shell comment or a quoted heredoc of a tool body is still a reference, and still refused.

## Not decided here

- [#3481](https://github.com/SocialGouv/iterion/issues/3481) — the residues above that a later change may move: a `best_effort` Verified Action as a producer that does not owe, the directive read-out on a recipe with a postcondition and on one line with the JSON, the join ordered by item index.
- A names-only event for the holes rule 2 does not refuse (#3421 lists it beside the narrow rule).
- The wide hardening. It needs the catalogue clean of partial mismatches (#3047), a strict failure that names a field without quoting its value (#3057), a census of what shipped bots read from the other namespaces, and a way for a human gate and a compute node to declare the field they may leave out.

## Witnesses this record owes

Through a real engine, the production executor, the production recovery recipes and the runner's classifier (`pkg/runner/tool_contract_test.go`): `TestAToolNodeThatPrintsBesideItsJSONFailsAtThatNode` (the incident: one execution, the next node never starts, no byte of the stdout in the failure), `TestAToolNodeAnsweringWithJSONThatIsNotAnObjectFails`, `TestAToolBodyReadingAFieldItsProducerOwesIsRefusedBeforeItRuns` (four bodies), `TestTheToolContractLeavesEveryOtherShapeAsItWas`, `TestEachToolContractDialRestoresTheLenientReading`, `TestAToolNodePrintingAnEngineDirectiveBesideItsJSONAnswersWithTheJSON`, `TestAFieldAHumanOrAComputeLeftOutIsNotOwed`, `TestAVerifiedActionOwesItsDeclaredFieldsOnItsSkipToo`, `TestAToolNodeInAFanOutBranchIsHeldToTheSameContract`. Beside them: `TestOnlyAToolProducerOwesADeclaredField`, `TestEveryRunBodyThatRendersOutputsAsksWhatItsProducersOwe` and `TestEveryDirectiveTheEngineScansIsOutOfTheAnswer` (`pkg/backend/model/tool_contract_test.go`), `TestTheOutputValidationDialTellsUnsetFromOff` and `TestAToolContractViolationClassifiesAsAPermanentToolFailure` (`pkg/runtime/tool_contract_dial_test.go`), `TestAJoinMergesCollidingBranchOutputsInBranchOrder` (`pkg/runtime/convergence_contract_test.go`), and `TestR2CatalogNodesThatPrintedBesideTheirAnswerNowConform` (`internal/sweep/runtimecatalog/round2_catalog_test.go`), which runs the fixed catalogue nodes themselves.
