# ADR-062: `<system-reminder>` envelope for harness-origin injections

- **Status**: Accepted
- **Date**: 2026-07-07
- **Authors**: Adry
- **Code**: [pkg/backend/model/system_reminder.go](../../pkg/backend/model/system_reminder.go) (`systemReminder`), [pkg/backend/model/generation.go](../../pkg/backend/model/generation.go) (steering injection), [pkg/backend/model/executor_template.go](../../pkg/backend/model/executor_template.go) (`PERMISSION GRANTED/DENIED`, `PRIOR INTERACTION`), [pkg/backend/model/claw_backend.go](../../pkg/backend/model/claw_backend.go) (no-tool / final-turn nudges)

## Context

Both backends inject harness-origin text into a running conversation:
operator/supervisor steering messages, resume prepends, tool-permission
grant/deny verdicts, and claw's own tool-use / final-turn nudges. All of
this is authored by iterion (the harness), not by the human operator and
not by the model.

Plain-text injection is a problem: to the model, harness-authored text is
indistinguishable from user-authored content. Models trained on the
Claude Code / claw prompt contracts are taught to read
`<system-reminder>…</system-reminder>` as harness-injected background —
but only when the envelope is actually applied. Injected without it, a
`PERMISSION GRANTED` verdict reads as ambient user chatter that may not
reliably gate the next tool call, and operator steering is treated as
low-priority background rather than a directive to act on.

## Decision

A single `systemReminder(text)` helper
([system_reminder.go](../../pkg/backend/model/system_reminder.go)) wraps
every harness-origin mid-run injection in
`<system-reminder>\n…\n</system-reminder>`. It is applied uniformly:

- operator/supervisor steering in
  [generation.go](../../pkg/backend/model/generation.go) (queued-message
  injection),
- resume prepends and `PERMISSION GRANTED` / `PERMISSION DENIED` /
  `PRIOR INTERACTION` verdicts in
  [executor_template.go](../../pkg/backend/model/executor_template.go),
- claw's no-tool and FINAL-turn nudges in
  [claw_backend.go](../../pkg/backend/model/claw_backend.go),

on **both** backends, so a bot behaves identically on claude_code or
claw. Content that must carry **user** authority — operator steering, the
`PERMISSION GRANTED/DENIED` verdicts the model must obey — states that
authority explicitly *inside* the envelope, so "this is background" and
"this is a directive from the operator" stay distinguishable while both
ride the same trained-recognised tag.

## Trade-offs

The envelope buys reliable model recognition of harness text — steering
gets acted on, permission verdicts gate the next call — at the cost of a
hard dependency on a specific provider convention (`<system-reminder>`).
The honest concession: the tag is not a wire-level guarantee; it works
because current target models were trained to honour it, and that
training is outside iterion's control.

## Alternatives considered

### 1. Keep plain-text injection (prior behaviour)

Inject steering, verdicts, and nudges as ordinary text with no envelope.

**Rejected because**: the model cannot distinguish it from user-authored
text. Operator steering is read as background and ignored, and
`PERMISSION GRANTED/DENIED` verdicts do not reliably gate the next tool
call — the injection is present but inert.

### 2. A bespoke per-injection marker (custom prefix / structured field)

Invent an iterion-specific marker (e.g. `[[HARNESS]]`) per injection
site.

**Rejected because**: the models are already trained on the
`<system-reminder>` contract from the Claude Code / claw prompts; a
novel marker earns none of that recognition and would have to be taught,
which iterion cannot do at inference time.

## Consequences

- **One helper, one envelope, both backends.** Every harness injection
  goes through `systemReminder`, so a bot's steering / permission /
  resume behaviour is identical across claude_code and claw.
- **Authority is explicit, not positional.** User-authority content
  (steering, permission verdicts) declares it in-envelope, so wrapping in
  a "background" tag does not accidentally demote a directive.
- **Recognition depends on model training.** The wrapper is only as good
  as the target model's `<system-reminder>` training. A model that drops
  that training silently weakens every injection.
- **Re-challenge**: if a target model drops `<system-reminder>` training,
  or providers standardise a different injection-envelope convention, the
  wrapper tag must change (a one-line edit in `systemReminder`).
