# ADR-100: The dry run at the executor seam

**Status:** Accepted (2026-09-16, lot 4 of #1010, PR #1292)
**Relates to:** ADR-098 (the authoring surface), ADR-099 (contracts), the framing plan's dispositions F11, F12, F13

## Context

A `.bot` that compiles still dies at its first paid run, in ways the compiler
cannot see: a `compute` that divides into an `int`, a `command:` the shell
refuses, a `{{outputs.x.y}}` of a node that has not run on this path, a loop
spent with no exit. This is the runtime class of failure — class R in the
authoring programme — and it is the most expensive one: a sandbox, a clone
and a phase of planning are paid before the death.

The programme promised a way to meet those deaths before the run. The plan's
adversarial review settled three things about that promise before a line was
written: the e2e scenario executor is not a validator and must not be
exposed as one (F11); shapes and two passes are not coverage, so the promise
is "reduces R", never "kills R" (F12); and a tool's `command:` runs under
`bash -c` while a `script:` runs the interpreter its `language:` names, so a
syntax check must hold text to the interpreter that will read it (F13).

## Decision

1. **The dry run is an implementation of `runtime.NodeExecutor`**
   (`pkg/dryrun`), never a mode of the engine. It renders what a node would
   send — prompts, `command:`, `script:`, `postcondition:` — through the
   production renderers (`model.TemplateResolver`, `RenderCommand`,
   `RenderScript`), holds shell text to its interpreter's parser, and answers
   with a fixture or a shape of the node's output schema. Nothing reaches a
   model, a shell or the workspace; the store and the working directory are
   temporary directories of the run's own.
2. **Every short-circuit the engine needs is a named option**
   (`runtime.WithSimulation(Simulation{AnswerHumans, EventsArrive,
   AnswersArrive})`), read at one site each, and a sweep test proves no
   production package passes it. There is no `if dryrun` in the engine.
3. **Two passes, bias true then false**, so every `when` is met on both
   sides; the report names the nodes and edges no pass reached, the nodes
   whose output was only a shape, and each pass's end — a death, or a
   `fail` node the bot declares, read apart. A child a `subbot` hands work
   to is read within the bundle's collection and simulated the same way; its
   death is the parent's.
4. **The renderer that leaves a hole is the one that names it.** An
   unresolved reference is reported by the resolver at the point it gives
   up, through a listener; the rendered text is never re-read for braces,
   because a value may carry `{{…}}` of its own and that is the value.
5. **The report never decides `valid`.** `validate --exec` runs only on a
   program that compiles, and its findings say what the first paid run would
   have met; `clean` is the report's verdict; the exit code is the
   compiler's — or, when a dry run was asked for and could not run, the
   dry run's own inability, said in `exec_error`.
6. **Alongside, the compiler names the two deaths it can see** — C145, a
   bounded loop with no exit at its cap; C146, a division into an `int`
   field outside `floor()`/`round()` — and the runtime names the loop death
   as `LOOP_EXHAUSTED`, the code the documentation always promised. `fmt`
   and `fix` close the loop on the text: canonical form and mechanical
   remedies, each proven the same program before it is written, each
   refusing by name what it cannot rewrite.

## What is not promised

- **Coverage.** A shape is not a value: a condition read from a shaped output
  decided nothing about the real bot, and the report says so by node.
  Fixtures narrow that, they do not close it.
- **The image.** A binary a `command:` names is not looked for in the sandbox
  image; a connector action is not executed; a `language:` without a checker
  is said unchecked. Separating syntax from binaries from arguments (F13) is
  what makes the first honest.
- **The registry.** A `bot://` child is not simulated; its output is a shape,
  said as such.
- **Unbounded loops.** Their fuel is their ceiling; C098 names the missing
  convergence condition, and the dry run runs them to the cap like the engine
  does.

## Alternatives rejected

- **`iterion run --stub`.** A second entry into the engine with a second
  reading of "simulation": one surface (`validate --exec`), one executor.
- **Exposing the e2e scenario executor.** Built to script a scenario, not to
  read a bot; it answers what a test tells it to, which is the opposite of a
  validator (F11).
- **Re-scanning rendered text for `{{`.** Reintroduces, one layer up, the
  cascade the renderer's single pass exists to prevent; a fixture carrying
  braces became a phantom finding in the first round.
- **Making the dry run's findings fail validation.** The compiler's verdict
  is about the program; the dry run's is about a run that did not happen.
  Mixing them would make a shaped `when` an error.

## Consequences

`SKILL.md` and `docs/dsl.md` teach the three steps — `validate`, `validate
--exec`, `run` — with `fmt` and `fix` around them. The catalogue's ten
bounded loops with no exit at their cap are named by C145 and left to their
authors (#1293). The retry policy classifies `LOOP_EXHAUSTED` as
deterministic; a run that dies of it is not retried by the fleet.
