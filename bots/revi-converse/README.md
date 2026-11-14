# revi-converse (Revi (converse)) — answers a follow-up question in a PR discussion thread

Conversational sibling of Revi (`review-pr`). When an authorized forge user
asks a focused question on an open pull request (merge request on GitLab) —
`/revi why is the SSRF critical?` — this bot reads the question plus the PR
diff against its merge-base, writes one concise, `file:line`-grounded answer,
and posts it as a **reply in the same discussion thread** via the
`forge_token`. The model only drafts; a deterministic step posts. It never
edits, fixes, or commits code.

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
  → reads the operator's question + `git diff $(git merge-base base_ref HEAD)`
    (and the thread, read-only, when the transcript is missing), drafts the
    answer and hands it back in `converse_output.answer` — it never posts
publish_reply   (tool, deterministic python script — no LLM)
  → reads the thread once (a note that is an exact copy of this run's reply
    means it already posted), posts the draft with the forge_token, then
    re-reads the created note by its id; emits posted, reply_url and a
    skipped_reason saying why not
converse_health (tool, deterministic — no LLM)
  → asserts the invariant: degraded=true unless posted=true with an http(s)
    reply_url on the host of `pr_url`, plus a banner (UNVERIFIED, the
    skipped_reason, or MISSING); the run still succeeds
done
```

The agent's model is `${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5-5}` and its
effort `${ITERION_VIBE_EFFORT_CONVERSE:-medium}`. Its tool loop only reads:
a question that runs out of steps still hands back its draft (claw's recovery
pass), and the draft is still posted — posting no longer depends on how many
steps the model spent reading (VAO !48: a reply that took 61 steps was
drafted and never posted). A drafted answer is posted whatever the agent's
`skipped_reason` says: the field is required, and a step-limited model fills
it ("N/A"). The reason is the banner when the draft is empty — or, beside the
reason, only filler ("N/A", "none", "-", "null", no run of three letters) —
and it is carried into `publish_reply`'s summary whenever the agent gave one.

`publish_reply` reads the forge from `pr_url`'s path and posts where each
forge's REST API takes a reply:

| `pr_url` | `discussion_id` | Posts to |
|---|---|---|
| GitHub `…/<o>/<r>/pull/<n>` | set (review-thread reply lane) | `POST /repos/<o>/<r>/pulls/<n>/comments/<id>/replies` |
| GitHub | empty (`/revi <question>` PR comment) | `POST /repos/<o>/<r>/issues/<n>/comments` |
| GitLab `…/<project>/-/merge_requests/<iid>` | set (both GitLab lanes) | `POST /projects/<project>/merge_requests/<iid>/discussions/<id>/notes` |
| GitLab | empty | `POST /projects/<project>/merge_requests/<iid>/notes` |
| Forgejo `…/<o>/<r>/pulls/<n>` | ignored | `POST /repos/<o>/<r>/issues/<n>/comments` |

The API base is `https://api.github.com` for github.com, `<base>/api/v3`
for a GitHub Enterprise host, `<base>/api/v4` on GitLab, `<base>/api/v1`
on Forgejo, where `<base>` is `forge_base_url` when the webhook pinned one —
`pr_url` must then sit on its host and under its path — and `pr_url`'s origin
otherwise. A GitLab or Forgejo served under a sub-path is reached only
through a webhook provisioned from a forge connection, whose base URL keeps
the path: a base pinned through the webhook API is stored as scheme://host
(`validateForgeBaseURL`), and reaches the run as the origin. Auth is `Authorization: Bearer` (GitHub, GitLab) or `token`
(Forgejo); redirects are not followed. The script receives every value as a
JSON literal in its temp file, so no size or byte (a NUL is dropped before
posting) keeps it from running. Before posting, the body loses its
carriage returns; on GitLab, a line opening on `/` then a letter gets a
backslash (the quick-action guard, #2422); on every forge, the first line
the webhook's command grammar reads gets one too when it opens on `/`, so the
bot's reply never re-fires `/revi`. An unclosed code fence is closed, an
answer past 60000 UTF-16 units is cut with a visible marker, and the run
footer ([run-link](../../docs/agents/bots/run-link.md)) is appended — never
cut. Every failure (no token or an unreadable one, an unusable `pr_url`, the
forge's status and message, a re-read that misses) is `posted=false` with its
reason (capped at 2000 characters, NUL bytes dropped from every field —
they ride `converse_health`'s environment), exit 0.

**The forge token is not posted in a body.** Two guards. `forge_token` is
granted the `file` sink only, so a `__ITERION_SECRET_forge_token__` the agent
quotes — planted in a public thread, a diff or a question — is defused by the
engine before the script runs and never materialised: the placeholder path is
closed whatever the script checks. And the script refuses a text carrying, as
written or as a forge renders it (HTML comments and tags dropped, entities
decoded): the token's placeholder, raw or defused; the token; its percent,
base64 (standard or URL-safe, padded or not) or hex encoding; and, for a
token of 16 letters and digits or more, the token, its base64 or its hex (any
case) with anything but letters and digits between their characters. That
list is what it refuses: an agent that read the token file and encodes it
another way is not recognised. On a refusal the draft is withheld from the
report.

## Configuration

| Var | Type | Default | Meaning |
|---|---|---|---|
| `workspace_dir` | string | `${PROJECT_DIR}` | The repo checkout the agent diffs and reads. |
| `base_ref` | string | `main` | Branch comparison base; the agent diffs `merge-base base_ref HEAD`. The webhook handler overrides it from the PR's target branch. |
| `pr_url` | string | `""` | The PR/MR URL the conversation lives on; `publish_reply` derives the forge, its API base, the project and the number from it, and `converse_health` counts a `reply_url` only on its host. Manual runs MUST pass it. |
| `discussion_id` | string | `""` | The thread id to reply in. Empty posts a new PR comment (GitLab: an MR note) instead. |
| `forge_base_url` | string | `""` | The webhook's pinned forge web base, stamped by the webhook lanes when the config names one. `publish_reply` derives the API from it; empty derives it from `pr_url`'s origin. A sub-path survives only from a forge connection's base URL (a pin set through the webhook API keeps scheme://host). |
| `trigger_note` | string | `""` | The operator's raw note body, including the leading slash-command. |
| `converse_question` | string | `""` | The question itself — the args after `/revi`. Empty falls back to `trigger_note`. |
| `thread_context` | string | `""` | The discussion transcript fetched and injected by the webhook handler. Empty on manual runs; the agent then reads the thread itself (a GET). |
| `replier` | string | `""` | Forge username of the operator who asked, used to address the answer. |

The studio launch form renders `converse_question` and `pr_url` top-level;
`discussion_id`, `trigger_note`, `thread_context`, `replier` and
`forge_base_url` are webhook plumbing hidden from the form (still settable
via `--var`).

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

Run without the `forge_token` to compose an answer without posting it;
inspect it with `iterion report --run-id <id>` (the draft is the agent's
`answer`). Such a run reads degraded: nothing answered the question on the
forge, and `converse_health`'s banner is `publish_reply`'s `skipped_reason`.
Omitting `discussion_id` posts a new PR comment instead of a thread reply.

## Notable

- **Secret**: `forge_token`, mounted `as: file`, `optional: true`,
  `sinks: [file]` (ADR-146; `requires.iterion >= 4.7.0` reads the grant).
  `publish_reply` reads it by the path its script renders
  (`{{secrets.forge_token.path}}`); the agent reads the same file only for
  the read-only thread GET — never into a prompt. Same binding name as
  `review-pr`, so one org-level binding resolves both, and the same forge
  identity the webhook's loop guard recognises. Missing token ⇒
  `posted=false` with a `skipped_reason`. Requested scopes:
  `pull_requests: write`, `repository: read`.
- **Workflow block**: `repo_devbox: off` (answering a question about a diff
  reads the repo, it never builds it); no `worktree:` and no `sandbox:` are
  declared. Budget: `max_parallel_branches: 1`, `max_duration: 30m` (the
  agent's 60-step loop at up to ~30 s a step on the gateway), `max_cost_usd:
  5`.
- **Read-only by construction**: the only write is `publish_reply`'s POST.
  No board writes, no source mutation.
- **Deferred**: a `forge.reply` DSL capability (sibling of `board.create`)
  would let the server own the posting instead of the bot's node —
  [docs/forge-conversations.md](../../docs/forge-conversations.md) §A4.
