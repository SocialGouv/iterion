# ReArchi (`adr-rechallenge`)

Human-in-the-loop **re-challenger** for a single Architectural Decision
Record. ReArchi loads ONE ADR + the current code state, surveys what
drifted since the decision was made, frames the case for revisiting it,
and asks you one question with three options.

## The decision

```
load_adr -> survey_code -> frame_arguments -> human_decision
                                                  |
                                  ┌───────────────┼───────────────┐
                                 keep            change          addendum
                                  |               |                |
                                done   file_change_ticket   write_addendum
                                                |                |
                                              done       human_commit_gate
                                                         /              \
                                                       commit            skip
                                                         |                |
                                                  commit_changes        done
```

| Branch | What happens |
|--------|--------------|
| **keep** | End the run. No change, no commit. The ADR stays as-is. |
| **change** | File ONE backlog ticket on the native board with the proposed change + the rationale + a pointer back to the ADR. You triage it. |
| **addendum** | Append a short dated re-challenge note to the ADR, then ask you a second question: commit the note, or skip? On `skip`, the addition stays in your working tree for you to inspect by hand. |

ReArchi **never edits code**. The only file it may write to is the ADR
itself, only in the `addendum` branch, only to append a dated block.

## Run it

```bash
# CLI
iterion run bots/adr-rechallenge/main.bot \
  --var adr_path=docs/adr/008-bot-golden-replay-framework.md

# Studio: open bots/adr-rechallenge/main.bot -> Launch, fill adr_path.
```

Both human gates pause the run (`paused_waiting_human`); answer in the
studio form or from the terminal:

```bash
iterion resume --run-id <id> --file bots/adr-rechallenge/main.bot \
  --answer decision=addendum --answer rationale="<one-line context>"
```

## Required var

| Var | Required | Default | Notes |
|-----|----------|---------|-------|
| `adr_path` | yes | — | Repo-relative ADR path, e.g. `docs/adr/008-bot-golden-replay-framework.md`. |
| `workspace_dir` | no | `${PROJECT_DIR}` | Workspace root. Do NOT override on a sandboxed run (see CLAUDE.md dogfood note). |
| `adr_dir` | no | `docs/adr` | Prose context only; not used as a scope glob. |
| `scope_notes` | no | `""` | Free-form context from a dispatched issue body. |
| `issue_id` | no | `""` | Set by the dispatcher; empty for manual CLI runs. |

## Dispatched usage

The dispatcher picks up ReArchi via the manifest's `dispatch_vars`. Two
ways:

1. **Triaged ticket** — the `adr-cartograph` (Adry) bot files a
   `type:adr-rechallenge` issue with `bot_args.adr_path` set; the
   dispatcher routes it to ReArchi.
2. **Hand-filed ticket** — open an issue titled `Re-challenge ADR-NNN`,
   set `bot: adr-rechallenge` and `bot_args.adr_path` in the issue's
   custom fields, drop it in the dispatcher's eligible state.

## Knobs (env vars)

| Var | Default | Effect |
|-----|---------|--------|
| `REARCHI_MODEL_CLAUDE` | `claude-opus-5` | model for the survey + framing + branch agents |
| `REARCHI_EFFORT` | `high` | reasoning effort across the run |

For a cheaper / faster pass (e.g. a quick "any signal at all?"
sanity check), lower the effort.

## How re-challenge framing stays honest

The argument framing has a single rule:

> Every claim cites exactly one concrete piece of evidence: a file
> path, a commit hash, a dependency version, or a calendar date.
> Speculation ("maybe X is now better") is forbidden.

A `keep` outcome after a thorough survey is a successful run — there
is no penalty for "no signal worth raising". See
`skills/argument-framing.md` for the full contract and
`skills/rearchi.md` for the operating playbook.

## What ReArchi is NOT

- Not an ADR designer — the `change` branch only files a ticket; the
  replacement decision is designed elsewhere.
- Not a batch tool — one ADR per run by design. Loop the bot over a
  list via your shell or the dispatcher for batch operation.
- Not a code refactorer — the addendum is a markdown paragraph, not a
  code change. Use a downstream `feature_dev` / `bmady` run for the
  actual code work proposed in a `change` ticket.
