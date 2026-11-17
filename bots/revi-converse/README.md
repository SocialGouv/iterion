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
converse_scope  (tool, deterministic python script — no LLM, the entry node)
  → computes the diff facts the answer grounds in: the reviewed head, the
    merge-base with a best-effort refreshed base_ref (a failed fetch says
    base_is_current: false in the block), workspace anomalies, the changed
    files with their stat, and the hunk the thread's anchor names — one
    precomputed SCOPE block, capped at 48000 characters with a visible
    marker; its facts repeat review-pr's diff_precheck (the bots suite
    runs both scripts on the same fixture repos and compares the common
    ones), and changed_files: -1 stays the "scope unknown" sentinel
converse_agent  (agent, claude_code, session: fresh)
  → reads the SCOPE block, the thread and the operator's question, drafts
    the answer and hands it back in `converse_output.answer` — it never
    posts; what it could NOT verify (its step bound cutting the reads
    short, a failed fetch) rides `converse_output.unchecked`
publish_reply   (tool, deterministic python script — no LLM)
  → reads the thread once (a note that is an exact copy of this run's reply
    means it already posted), posts the draft with the forge_token —
    rendering the agent's `unchecked` as a visible "Lecture partielle"
    note — then re-reads the created note by its id; emits posted,
    reply_url and a skipped_reason saying why not
converse_health (tool, deterministic — no LLM)
  → asserts the invariant: degraded=true unless posted=true with an http(s)
    reply_url on the host of `pr_url`, plus a banner (UNVERIFIED, the
    skipped_reason, or MISSING) — or a reply withheld in evaluation mode,
    which reads `evaluation mode`, neither OK nor degraded; the run still
    succeeds. It reads nothing of `unchecked`: an admission is not a claim
done
```

The agent's model is `${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5-5}` and its
effort `${ITERION_VIBE_EFFORT_CONVERSE:-medium}`; a routing cell's `effort`
outranks that dial, and the team-gateway routing block sets `high` (#3283).
Its whole surface is the four tools it declares — `bash`, `read_file`, `glob`,
`grep` — and no MCP server: the node opts out of the ambient set (`mcp:
inherit: false`), so neither a plugin's servers nor the repository's
`.mcp.json` reach its requests — on claw, on claude_code (whose own scopes
stay shut: `--strict-mcp-config` holds for this node whatever
`ITERION_CLAUDE_CODE_STRICT_MCP` says); codex, which cannot switch off the
servers of its own configuration, refuses the node when that configuration
lists one — and it has no web tool. It answers from the diff and
the repository. Its tool loop is bounded at **20 steps** (was 60; the
re-cut window stays within [20,24] — a raise past 24 quietly re-inflates
the per-question cost the bound caps): the SCOPE block already carries the
diff facts, so the loop spends its steps on what the block lacks —
surrounding code, the sources a claim cites — and running out still hands
back its draft (claw's recovery pass), still posted — posting no longer
depends on how many steps the model spent reading (VAO !48: a reply that
took 61 steps was drafted and never posted).

**What the bound costs, said up front (#3283 D8):** a draft at the bound is
a *partially-read* answer, and the run says so three ways. The agent fills
the new `converse_output.unchecked` field with what it did not get to;
`publish_reply` renders it in the thread as a visible
`> Lecture partielle : …` blockquote, screened by the same guards as the
answer (credential refusal, quick-action guard, size cap); and the
measurement counts runs stopped by the bound — `formatting_pass_used` on
`delegate_finished` — rather than trusting any gate to infer it.
`converse_health` reads nothing of `unchecked`: an admission is not a
claim, and a partial answer is still a posted, verified reply. Beside an
empty draft no note is rendered — the reason speaks for that one. The
system prompt carries no `{{vars.*}}` at all: the workspace path, the base
ref and every per-run fact live in the user message (scope, thread,
question, path, in that order), so the provider's prefix cache serves the
next run from this one's bytes (#3283 S3 — the cross-run prefix; the
engine half is claw's ambient AGENTS.md header spelled relative to the
workspace). The workspace is named in the user message as the directory
the shell starts in (claw guarantees the cwd), so the prompts say plain
`git diff`, not `git -C <path>`.

A drafted answer is posted whatever the agent's
`skipped_reason` says: the field is required, and a step-limited model fills
it ("N/A"). The reason is the banner when the draft is empty — or, beside the
reason, only filler ("N/A", "none", "-", "null", no run of three letters) —
and it is carried into `publish_reply`'s summary whenever the agent gave one.

**What byte-stability does and does not cover** (the cross-run prefix, said
exactly): stable — the system blocks and the tool schemas for two checkouts
of one tree, both ambient shapes (the fallback's own AGENTS.md and a
CLAUDE.md whose `@AGENTS.md` import pulls it — iterion's committed spelling,
the flagship lane), at the sandbox secret mount. Not stable, said where they
live: a HOST (unsandboxed) run materializes secret files into a per-run temp
dir whose path enters the prompt (S3 did not set out to fix that; sandbox
runs — the measured lane — bind-mount the stable path); and a workspace that
is a SUB-DIRECTORY of its repository gets the claw walk's `Ancestor (<abs
path>)` labels for instructions sitting above it — run workspaces are
checkout roots, so webhook lanes never hit it; manual sub-dir runs do. The
wire witness compares full tool defs, not names.

**Lane notes on the anchor vars** (round-1 review): the lanes set the six
`thread_anchor_*` keys on every pr-surface launch they build — a bare
`/revi` (the review-pr route) and any other command bot receive them too and
simply never read them; the vars merge BEFORE the webhook's var layers, so
an operator's pinned `thread_anchor_*` wins over a payload's anchor and an
empty anchor clobbers nothing. Forgejo/Gitea payloads of the review-comment
event carry the fields Gitea implements (the file path, when any): a payload
that names no side has no current line to focus, and degrades to the file's
whole diff, said in the block.

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

**Evaluation mode** (`--var publish_mode=off`, #3283) measures a question's
cost and answer on a live thread without answering in it. The agent drafts as
always; `publish_reply` reads the draft, checks the target, builds the body
and refuses a credential in it, then stops before its first request: the
forge sees nothing — no duplicate read, no POST — and no token is needed. It
reports `posted=false`, `evaluation=true`, a `skipped_reason` opening on
`evaluation mode (publish_mode=off)` and a summary naming the call it
withheld; `converse_health` reads that as `evaluation mode`, not degraded and
never OK. A check that would have stopped a real post (an empty draft, a
`pr_url` or discussion id it cannot reply on, an answer carrying the token)
still speaks first, under its own reason. Operators set the var — a
launch's `--var` or `bot_args`, a remote launch, an integration's pinned
launch vars — and so can a bot holding `board.create`, through a card's
`bot_args`. A pull request's author or a webhook payload never can: the
webhook lanes build their vars from fixed payload keys, and no manifest
channel they layer (`forge.webhook.launch_vars`, an invocation's `args_var`
or `context_vars`, `dispatch_vars`) may carry it in any bot of the catalogue
(`bots/publish_mode_eval_test.go`). The server reads the literal — a `$`
expansion is refused at its launch admission — and holds the run to it: its
merge-gate arms owe it nothing, it mints it no publish grant, and its
hand-off walk skips it ([merge gate](../../docs/merge-gate.md#evaluation-mode)).

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
| `thread_anchor_path` | string | `""` | The thread ROOT's anchor file (the review comment the thread hangs on), set by the webhook lanes on both GitLab entries and the GitHub/Forgejo review-thread reply. Empty when the thread hangs off the conversation or the forge names no position. |
| `thread_anchor_old_path` | string | `""` | The file before a rename, when the forge reports one (GitLab's `old_path`). |
| `thread_anchor_line` | string | `""` | The CURRENT line the anchor renders on (absent on an outdated comment). |
| `thread_anchor_old_line` | string | `""` | The PRE-IMAGE hint the forge keeps (GitHub's `original_line`, GitLab's paired old line) — never read as a current line. |
| `thread_anchor_side` | string | `""` | `"new"` (the post-image line) or `"old"` (a line the change removed). |
| `thread_anchor_commit` | string | `""` | The commit the anchor was taken on. `converse_scope` isolates the anchored hunk only when it names the head under review; stale, removed-side or line-less it shows the file's whole diff and says so. |
| `publish_mode` | string `""` \| `"off"` | `""` | Evaluation mode: `off` drafts the answer and sends nothing to the forge (see above). Set by an operator or a bot holding `board.create`, never by a webhook payload. |

The studio launch form renders `converse_question` and `pr_url` top-level;
`discussion_id`, `trigger_note`, `thread_context`, `replier`,
`forge_base_url`, the six `thread_anchor_*` keys and `publish_mode` are
hidden from the form (all still settable via `--var` or `bot_args`). A
manual run can pass the anchor vars too — `--var
thread_anchor_path=app.go --var thread_anchor_line=42 --var
thread_anchor_side=new --var thread_anchor_commit=<head-sha>` focuses the
scope the same way a webhook thread does.

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

Pass `--var publish_mode=off` to compose an answer without posting it;
inspect it with `iterion report --run-id <id>` (the draft is the agent's
`answer`). Such a run reads `evaluation mode`: nothing reached the forge, by
design. A run without the `forge_token` and without the var does not post
either, but reads degraded: `converse_health`'s banner is `publish_reply`'s
`skipped_reason`.
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
  agent's 20-step loop at up to ~30 s a step on the gateway, with the
  deterministic steps on top), `max_cost_usd: 5`.
- **Read-only by construction**: the only write is `publish_reply`'s POST,
  withheld in evaluation mode (`publish_mode=off`). No board writes, no
  source mutation.
- **Deferred**: a `forge.reply` DSL capability (sibling of `board.create`)
  would let the server own the posting instead of the bot's node —
  [docs/forge-conversations.md](../../docs/forge-conversations.md) §A4.
