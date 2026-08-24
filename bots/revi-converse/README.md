# revi-converse (Revi (converse)) — answers a follow-up question in a PR discussion thread

Conversational sibling of Revi (`review-pr`). When an authorized forge user
asks a focused question on an open pull request (merge request on GitLab) —
`/revi why is the SSRF critical?` — this bot reads the question plus the PR
diff against its merge-base, writes one concise, `file:line`-grounded answer,
and posts it as a **reply in the same discussion thread** via the
`forge_token`. It never edits, fixes, or commits code.

Each reply is one short, idempotent run: the bot holds no state, the forge
thread is the conversation state. See
[docs/forge-conversations.md](../../docs/forge-conversations.md) for the
webhook spine (note parsing, replier authorization, loop guard, thread
transcript injection) this bot sits on — it is step A5 there.

## When to use it

- An operator asks a follow-up on an open PR about Revi's earlier findings or
  about the diff itself: clarification, rationale, severity justification,
  alternative fixes.
- **Not** for reviewing the PR — that is `review-pr` (Revi), which produces
  the findings. revi-converse answers questions *about* an existing review or
  diff and posts a single note; it never emits a fresh review, board issues,
  or a merge-gate status.
- **Not** for editing code (Billy / Featurly), and not for board triage.

The two split cleanly on the same slash command: `review-pr` declares
`disambiguator: when_args_empty`, so a bare `/revi` re-reviews; revi-converse
declares `when_args_present`, so `/revi <question>` routes here.

## How it runs

```
converse_agent  (agent, claude_code, session: fresh)
  → reads the operator's question + `git diff $(git merge-base base_ref HEAD)`,
    drafts the answer, POSTs it into `discussion_id` per skills/forge-reply.md,
    re-fetches the thread to VERIFY the note landed, emits `converse_output`
converse_health (tool, deterministic — no LLM)
  → asserts the invariant: posted=true with an empty reply_url ⇒ degraded=true
    plus a loud banner; the run still succeeds
done
```

The agent's model is `${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5}` and its
effort `${ITERION_VIBE_EFFORT_CONVERSE:-medium}`. The system prompt bans a
reply body that *begins* with the literal `/revi` (or `/ask`) token — that
would re-fire the webhook and loop forever; `skills/forge-reply.md` restates
the rule and gives the per-forge REST calls (GitLab discussion notes, GitHub
review-comment replies with an issue-comment fallback, Forgejo/Gitea issue
comments).

## Configuration

| Var | Type | Default | Meaning |
|---|---|---|---|
| `workspace_dir` | string | `${PROJECT_DIR}` | The repo checkout the agent diffs and reads. |
| `base_ref` | string | `main` | Branch comparison base; the agent diffs `merge-base base_ref HEAD`. The webhook handler overrides it from the PR's target branch. |
| `pr_url` | string | `""` | The PR/MR URL the conversation lives on; the forge host, project and iid are derived from it. Manual runs MUST pass it. |
| `discussion_id` | string | `""` | The thread id to reply in. Empty disables posting (the agent still composes an answer for the report). |
| `trigger_note` | string | `""` | The operator's raw note body, including the leading slash-command. |
| `converse_question` | string | `""` | The question itself — the args after `/revi`. Empty falls back to `trigger_note`. |
| `thread_context` | string | `""` | The discussion transcript fetched and injected by the webhook handler. Empty on manual runs; the agent then fetches the thread itself. |
| `replier` | string | `""` | Forge username of the operator who asked, used to address the answer. |

The studio launch form renders `converse_question` and `pr_url` top-level;
`discussion_id`, `trigger_note`, `thread_context` and `replier` are webhook
plumbing hidden from the form (still settable via `--var`).

## Invocation

The manifest registers one invocation: a `command` of name `revi`, scope
`pr`, `mode: direct`, `args_var: converse_question`, `disambiguator:
when_args_present`, `min_replier_role: developer`. The forge block declares
the `pull_request_comment` event only — there is no PR-open auto-trigger.
Manifest `triggers:` are `revi-converse`, `ask`, `converse`.

Manually:

```bash
iterion run bots/revi-converse/main.bot \
  --var workspace_dir=/path/to/repo \
  --var base_ref=main \
  --var pr_url=https://github.com/owner/repo/pull/42 \
  --var converse_question="why is the SSRF finding critical?" \
  --var discussion_id=<thread-id>
```

Omit `discussion_id` (or run without the `forge_token`) to compose an answer
without posting it; inspect it with `iterion report --run-id <id>`.

## Notable

- **Secret**: `forge_token`, mounted `as: file` and `optional: true`, read at
  `/run/iterion/secrets/forge_token` and passed to curl by path — never into a
  prompt. Same binding name as `review-pr`, so one org-level binding resolves
  both. Missing token ⇒ `posted=false` with a `skipped_reason`. Requested
  scopes: `pull_requests: write`, `repository: read`.
- **Workflow block**: `repo_devbox: off` (answering a question about a diff
  reads the repo, it never builds it); no `worktree:` and no `sandbox:` are
  declared. Budget: `max_parallel_branches: 1`, `max_duration: 10m`,
  `max_cost_usd: 5` — conversations must feel snappy.
- **Read-only by construction**: the only write is the curl POST that
  publishes the answer. No board writes, no source mutation.
- **Deferred**: a `forge.reply` DSL capability (sibling of `board.create`)
  would let iterion own the posting instead of curl in a skill —
  [docs/forge-conversations.md](../../docs/forge-conversations.md) §A4.
