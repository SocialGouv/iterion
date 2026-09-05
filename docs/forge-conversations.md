# Forge conversations — replying to the bot (forge threads → run → in-thread reply)

How an authorized forge user "talks back" to a bot (reply to its review,
ask a question, or `/revi` for a re-review) and gets a response **in the
same discussion thread**. This is the conversational layer on top of the
auto-review webhook spine. It reuses three patterns already in the tree —
the webhook override model ([byok.md](byok.md)), the capability model
(`board.*`), and runtime input injection (`RepoURL`/`RepoSHA`) — so the
DSL stays small.

Status: A1 (note parsing), A2 (handler + authz + loop-guard +
reply-in-thread trigger), A3 (conversation vars incl. the fetched thread
transcript as `thread_context`) and A5 (`revi-converse`) are shipped —
on **GitLab** (notes) and on **GitHub** (review threads, below).
A4 (`forge.reply` capability) is the remaining deferred step — the reply
POST is skill-based (`curl`) until then.

## GitHub — replying inside a review thread

GitHub splits PR comments across two wire events, so the lane has two
entries there:

- **Reply to an inline suggestion** (`pull_request_review_comment`,
  action created): `handlePRForgeReviewThreadReply`
  ([pkg/server/webhooks_prforge.go](../pkg/server/webhooks_prforge.go))
  filters (open PR, event/project allowlists), drops thread-OPENING
  comments from the payload alone (`in_reply_to` empty ⇒ nobody is in
  that thread yet — every inline comment of a bot review echoes back
  as one, so this spares the whole fetch), drops fork PRs the same
  payload-only way (`SameRepoAsBase` — the base clone URL and the head
  ref would not name one repository), runs the loop-guard next,
  still without forge I/O (`isIterionForgeBotAuthor` — the bot's
  own answer echoes back as this very event), requires the converse
  bot in the webhook scope (`roleBots().ReviConverse` +
  `cfg.AllowsBot`), then gates: the thread is fetched
  (`ListPRReviewComments` — newest-first capped pagination, handed
  back chronological, so a long-lived PR's cap never blinds the gate
  on the thread just replied to) and must contain a
  comment by the bot identity — a human↔human thread never triggers —
  and the replier must clear `authorized_repliers` or
  `min_replier_role`. The launch carries `converse_question` (the
  reply body), `thread_context` (the thread transcript, bot entries
  labelled, capped by the same 16k anchor+newest budget as the GitLab
  lane — `webhooks.CapTranscript`) and
  `discussion_id` = the **thread root** comment id — exactly what
  GitHub's `/pulls/{n}/comments/{id}/replies` endpoint wants (the
  bot's `forge-reply.md` §4). Idempotency: one launch per reply
  comment (`rc|…` key space).
- **`/revi <question>` as a plain PR comment** (`issue_comment`): no
  special-casing — the generic command registry routes it. The
  manifests declare complementary disambiguators
  (`review-pr` `when_args_empty`, `revi-converse` `when_args_present`
  + `args_var: converse_question`), the provision derives the
  two-route `command_map`, and `ResolveCommandRoute` picks by args
  presence. Bare `/revi` stays a re-review.

Enablement is provisioning, not code: the review-thread firehose is
its OWN normalized manifest event, `pull_request_review_comment`
([pkg/forge/event_map.go](../pkg/forge/event_map.go)), declared by
`revi-converse` — deliberately NOT folded into `pull_request_comment`,
which nine catalog bots declare: one submitted review fires one wire
delivery per inline comment, each charged against the webhook rate
bucket and the org monthly quota before any handler filters it, so
only a repo whose `bot_ids` actually include the conversational bot
subscribes that volume. A (re-)provision with the converse bot
subscribes the hook and regenerates the config's `event_allowlist`
together; webhooks provisioned without it stay inert on review-thread
replies. Forgejo is deliberately not wired yet (its dispatch never
routes the event, and the normalized event maps to no Forgejo native
event).

## Model — stateless, the thread is the state

A reply fires a forge `note` webhook → iterion authorizes the replier →
launches **one short run** carrying the thread context → the bot reads the
thread + the diff and posts a reply **in the same discussion**. The
conversation state lives in the GitLab thread (the source of truth), **not**
in a paused run. Each reply = one idempotent, recoverable run. That is the
durability win: no `paused_waiting_human` runs held open across hundreds of
MRs.

> **Why not `interaction: human`?** Tempting (bot posts → run pauses → a
> GitLab reply resumes it), but it holds the run open indefinitely, needs a
> reply→run_id map, and is stateful (1 run = 1 conversation). Stateless
> reply-as-new-run is more robust for an event-driven, multi-MR system.
> `interaction: llm_or_human` stays useful for a different thing — the bot
> deciding to **escalate** to a real human.

## A2 — Webhook layer: note events + trigger + loop-guard

`pkg/webhooks/gitlab/note.go` (done) parses the `Note Hook`: `discussion_id`
(the thread to reply in), `note.body`, the author (`User`), and the MR
context. `ParsedNote.Command()` extracts a leading slash command;
`IsMergeRequestNote()` filters out issue/commit notes; `SubjectID()` is
`note:<id>` for idempotency.

The handler (`pkg/server/webhooks_gitlab.go`, dispatch on
`X-Gitlab-Event`):
1. A webhook opts into notes by adding `"note"` to its `event_allowlist`
   (default stays `merge_request`-only — safe).
2. **Loop guard (critical):** skip notes whose author is the bot itself
   (else the bot's reply re-triggers a run → infinite loop). Resolve the
   bot's forge user once from the forge_token (`GET /user`) and compare
   `author_id`; cache it per webhook.
3. **Trigger gate:** a note triggers when it carries a slash command or
   is a reply inside a bot-authored discussion thread. A command — `/revi`
   included — is routed generically through `webhooks.ResolveCommandRoute`
   over the webhook's `CommandMap`: the manifests' `when_args_empty` /
   `when_args_present` + `args_var` pair resolves a bare `/revi` to the
   reviewer and `/revi <question>` to the conversational bot, exactly as
   the GitHub `issue_comment` lane does. Only a plain reply-in-thread with
   no command stays bespoke, routed to the `revi_converse` role bot — and
   a provisioned but unresolvable converse bot is a visible
   `launch_error`, not a silent fallback to the reviewer. `/revi approve`
   uses the same helpers as GitHub: a maintainer floor (`approveFloor`,
   raise-only), self-approval refused, a forge error on the authorization
   read answered `200` + `launch_error`, an unauthorized replier silently
   `filtered`.
4. **Fork guard:** the note payload names no project ids, so the handler
   resolves the MR through the forge API and refuses a cross-project or
   unnamed head (`filtered`, 200); a resolution failure is a visible 502.
5. Idempotency on `note:<id>`; `MatchProject` as for MR events. The gates
   are thin wrappers over token-free cores (`gitlabCommandGateWithAPI`,
   `gitlabNoteGateWithAPI`), so their real logic is unit-tested rather
   than stubbed away.

## A2 — Authorization (the heart): two separate things

Do **not** conflate:
- **Who may trigger** = the *replier's* authorization (the GitLab user).
- **Under whose identity the bot posts** = the `forge_token` (the org
  binding, or the per-webhook secret override from [byok.md](byok.md)).

A replier is authorized when **(role-gate) OR (allowlist)**:
- **Role-gate (default):** the author has at least `min_replier_role`
  (e.g. Developer) on the project — checked via the forge API
  (`GET /projects/{id}/members/all/{user_id}` → `access_level`) using the
  resolved `forge_token`. Reuses GitLab's own permissions ("if you can
  push, you can talk to the bot") — no list to maintain.
- **Allowlist (explicit):** the author's username/id is in an explicit
  list — for collaborators who lack the role but should be allowed.

Both live at **org-default + per-webhook override**, exactly like the BYOK
key/secret overrides:
- `webhooks.Config.AuthorizedRepliers []string` (+ `MinReplierRole`)
  override the org-level defaults (a team setting).
- Validation/precedence mirror `validateKeyOverrides` /
  `validateSecretOverrides`.

An unauthorized note → `200 filtered`, no run (and an audit row).

## A3 — Conversation context (runtime injection, like RepoURL)

The webhook/runtime injects a standard structured input the way it injects
`RepoURL`/`RepoSHA`: a `Conversation` on `runview.LaunchSpec` →
`store.Run` (persisted for resume) → `queue.RunMessage` →
the engine vars. Fields: `thread_id` (discussion), `trigger_note`,
`replier` (username), `mr_url`, and optionally the fetched thread history.
Any bot becomes conversational with no per-bot plumbing — it just reads
`{{conversation.*}}`.

**Shipped (vars-level):** the handler injects `discussion_id`,
`trigger_note`, `trigger_command`/`trigger_args`, `replier` and — on the
converse route — `converse_question` plus `thread_context`, the discussion
transcript fetched from the forge API (chronological, the bot's own notes
labelled, capped at ~16k chars keeping the thread anchor + the newest
notes; see `gitlab.FormatThreadTranscript`). The gate fetches the
discussion ONCE and reuses it for both the reply-in-thread classification
and the transcript. The dedicated `Conversation` struct on `LaunchSpec`
(vs plain vars) remains the refactor to do when a second forge needs it.

## A4 — `forge.reply` capability (the DSL answer)

The DSL footprint is a **capability**, sibling of `board.create/move/read`
— not a new node type and not an `interaction:` mode. A bot declares
`capabilities: [forge.reply]` and the runtime opens a `forge_reply(thread_id,
body)` tool that posts in-thread via the `forge_token`, handling the forge
API + the bot identity + anti-replay. iterion controls the posting (vs raw
`curl` in a skill). Wire it like the board capability: an in-process claw
tool (`pkg/backend/tool/`) + the claude_code MCP path
(`__mcp-*` / HTTP), gated by the capability. Capability diagnostics extend
the C080–C082 family.

(The review-posting in `review-pr` can stay skill-based for now and migrate
to `forge.*` capabilities over time; the *reply* path starts capability-first
because it is short and security-sensitive.)

## A5 — The conversational bot

A `revi-converse` mode/bot (or a `reply` entry of `review-pr`): reads the
thread (`{{conversation.*}}`) + the diff, answers the user's note (or
re-reviews on `/revi`), and calls `forge_reply`. Reuses the
`forge-pr-review` skill family.

## Build order

A1 note parsing ✓ · A2 handler + auth (role-gate + allowlist, org-default +
per-webhook override) + loop-guard · A3 conversation injection · A4
`forge.reply` capability/tool · A5 `revi-converse` bot. Each is incremental
on the existing spine; A2 is the core (events + authorization).
