---
name: instrumentation
description: The observability-instrumentation playbook — how to survey a repo, what "done" means for the errors / logs / tracing families, the extend-don't-replace rule for logging seams, DSN opt-in semantics, and the verification discipline. Read this FIRST in every instrument campaign pass.
---

# instrumentation — the playbook

You are instrumenting ONE repository for observability. The families
requested for this run are in `scope`; this skill defines what each
family means and the discipline that keeps the work honest. Stack
specifics (which SDK, which log lib, which wiring seams) live in the
`lang-<stack>` skills — read the one(s) matching the repo. Tracing has
its own skill, read ONLY when scope asks for it.

## 0. Survey first (cheap, before any edit)

1. **Production entry points** — every place the code starts running
   for real: HTTP servers, daemons/long-running workers, queue/job
   consumers, schedulers, and the CLI `main`. These are where error
   capture and log-format defaults attach. List them; the list drives
   your todo.
2. **Existing logging** — does the repo already have a central logging
   module/wrapper? Grep for a `log`/`logger`/`logging` package of its
   own, then for strays (`print`, `console.log`, `fmt.Print*`,
   `System.out`, bare stdlib loggers) on production code paths.
3. **Configuration conventions** — how this repo reads env/config
   (prefix naming, a config loader, flag precedence). New env vars MUST
   follow the house convention; the DSN env var name is given to you
   and is the ecosystem standard unless the operator overrode it.
4. **Test layout + toolchain** — where unit tests live, how they run.
   Your instrumentation ships WITH tests; the deterministic gate will
   run them.

Mission notes from the operator override any default in this skill —
they are the repo owner's arbitration.

## 1. Family: errors (error tracking)

Target: an SDK speaking the **Sentry DSN protocol** — the same DSN and
SDK work against Sentry *and* GlitchTip (GlitchTip implements the
Sentry ingestion API; no GlitchTip-specific client exists or is
needed). Pick the stack's official Sentry SDK per the `lang-*` skill.

Definition of done:

- **Opt-in by env, off by default.** The SDK initialises ONLY when the
  DSN env var is set at runtime. Unset ⇒ strictly zero behaviour
  change: no goroutine/thread, no network, no init log noise beyond at
  most a debug line. Never hardcode a DSN; never invent a second
  activation path.
- **Loud, non-fatal init failure.** A malformed DSN or failed init is
  reported through the repo's own logger at error level — and the app
  keeps running. Observability must never take the service down.
- **Identity tags.** Set `release` (the repo's own version/commit
  source — a build-info package, a VERSION file, git describe) and
  `environment` (from the standard `SENTRY_ENVIRONMENT` env or the
  repo's own env notion). Without a release, events are unusable for
  regression triage.
- **Capture at the process seams**, not scattered call sites:
  - panic/uncaught-exception capture at every real entry point found
    in the survey (server request recovery, worker/goroutine recovery
    points, CLI top-level);
  - the fatal-exit path (an error that terminates the process is
    captured before exit);
  - existing recover/rescue blocks: add capture INSIDE them, do not
    restructure them.
- **Flush on shutdown.** Pending events are flushed with a short
  bounded timeout (~2s) on normal termination and on the fatal path.
  A captured event that dies with the process is a façade.
- **Scrubbing.** Configure the SDK's before-send hook to drop obvious
  secrets/PII (auth headers, tokens, DSN-like strings, emails where
  the repo treats them as personal data). Follow the repo's own
  sensitivity conventions when it has them.
- **Central wrapper, thin.** One small module owns init/flush/capture
  helpers so call sites never import the SDK directly in more than a
  handful of places. If the repo already HAS an error-tracking wrapper,
  extend it instead.

## 2. Family: logs (standardization)

Target: **one central logging seam** for the whole codebase, structured
and machine-shippable in production.

**THE RULE: extend, don't replace.** If the repo already has a central
logger — even a home-grown one — you extend IT (add the missing
format/hook/fields) and sweep the rest of the code onto it. Replacing a
working seam, or adding a parallel one, is the classic façade: two
half-adopted loggers are worse than one imperfect one. Only when the
repo has NO central seam do you introduce the stack's standard lib
(per the `lang-*` skill), wrapped thinly so the repo owns its API.

Definition of done:

- **Leveled + structured.** error/warn/info/debug levels and key-value
  fields (a `WithField`-style context or the lib's native equivalent).
  Log lines never crash the producer: a serialization failure is
  dropped or degraded, never thrown.
- **JSON by default in production.** Every long-running production
  surface (server, daemon, worker) emits one JSON object per line by
  default — stable field names (timestamp, level, msg, fields) suitable
  for Loki/ELK/CloudWatch. Interactive/TTY surfaces (the CLI a human
  runs) keep the human-readable format by default. BOTH are switchable
  by env/config following the repo's conventions — the default is a
  default, never a cage.
- **Stray sweep.** Production code paths log through the seam — sweep
  `print`/`console.log`/ad-hoc stdlib loggers into it. Out of scope:
  test files, throwaway scripts, developer tooling, and surfaces whose
  stdout IS the product (a CLI's command output is not a log).
- **Coupling to the tracker** (only when `errors` is also in scope):
  wire the seam's hook/handler mechanism so an **error-level log
  becomes a tracker event** (with its structured fields as context) and
  a **warn-level log becomes a breadcrumb** attached to subsequent
  events. The coupling lives in ONE place (the wrapper), is active only
  when the tracker is enabled, and must never block or panic the
  logging path.

## 3. Family: tracing (opt-in)

Only when `scope` includes `tracing`. Read the `tracing` skill — it
covers the Sentry-first transaction/span model, sampling, and per-stack
entry points. Never wire tracing "while you're at it".

## 4. Verification discipline

- **Unit tests, no network.** Every Sentry-protocol SDK has an
  in-memory/mock transport (see the `lang-*` skill) — assert that an
  error-level log or a captured panic produces an event with the
  expected fields, and that warn produces a breadcrumb. A test that
  needs a live DSN is wrong.
- **Format proof.** A unit test asserts the JSON log format's field
  names on a sample line (the shipper contract), and that the
  prod-surface default is JSON while the interactive default is human.
- **Off-state proof.** With the DSN env unset, init is a no-op — assert
  it (no client, no transport, no error).
- **Docs.** The DSN env var, the environment/release envs, the log
  format envs, and "how to point this at Sentry or GlitchTip" land in
  the repo's own docs, wherever it documents configuration. An
  undocumented env var does not exist.

## 5. Honesty

Your summary CITES the seams you wired (file:line), the env vars you
introduced, and the tests that prove each family. The deterministic
gate runs the repo's real build+tests behind you, and the adversarial
review reads your diff — claiming a family is wired when a seam is
missing costs you the next pass.
