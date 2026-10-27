# A forge-facing bot links its run

Anything a bot posts outside the deployment — a pull-request body, a review, a
verdict comment, a reply in a discussion — ends with the run that wrote it:

```
🤖 [iterion](https://iterion.cloud) · run [`<run id>`](<run url>)
```

Two reasons, and the second is the one that gets forgotten:

- **Evidence.** A verdict without its run is an assertion. The run page carries
  the steps, the models, the inputs, the cost — everything a reader needs to
  judge the text instead of trusting it.
- **Entry point.** The pull request is where someone meets this fleet for the
  first time, often on their own repository, with no idea what produced the
  diff. The link is the way in; without it the bot is an anonymous robot
  commenting on their work.

## The channel

`{{run.url}}` — the run-namespace member the engine resolves from the
deployment's public origin (`ITERION_PUBLIC_URL`, which the runner carries
beside the server), rendered as `<origin>/studio/runs/<id>`. It resolves in a
prompt, a tool `command:` / `script:`, an expression and a data mapping alike
([the run namespace](../../dsl.md#the-run-namespace)).

- **Deterministic node** (the body is python, the server publishes it): read
  the member into the script — `RUN_URL={{run.url}} RUN_ID={{run.id}}` — and
  append the footer where the body is assembled. `bots/dep-update-guard` and
  `bots/branch-improve-loop` are the worked examples.
- **Agent node** (the agent posts through the forge API itself, following
  `skills/forge-*.md`): the skill states the footer rule, the node's own user
  prompt renders both values. The rule without the address is unfulfillable
  prose, and the address without the rule is never used — the guard below
  checks both halves.
- **Server-published review** (`forge_publish_url`): `bots/review-pr` and
  `bots/gitops-warden` rebuild the page from the publish endpoint's own
  origin, which predates the member and stays truthful — the endpoint and the
  studio are the same deployment. New bots use the member.

## Three ways the link can lie, and what the footer does instead

**No usable origin.** `deeplink.PublicOrigin` refuses a value that is not an
absolute http(s) address, one holding whitespace, and one carrying userinfo —
each would publish something worse than nothing: a **relative** link (posted
on a forge it resolves against the *forge*, sending the reader to someone
else's 404), a destination truncated at the first space, or a credential in a
comment on a third party's repository. `{{run.url}}` is then the empty string
and the footer prints the run id alone. The reason is logged when an engine is built —
once per run, and once per subbot child — so a typo is a warning an operator
can read rather than a link that quietly went missing.

**A run id carrying markdown.** A run id is **caller-chosen** at launch
(`run_id` on the launch API) and screened only by
`store.SanitizePathComponent`, which rejects path traversal and control
characters — not `` ` ``, `[`, `]`, `(` or `)`. Pasted raw into the footer's
label, an id like `x](https:evil.example)` closes the link early and publishes
an attacker-chosen host **under the bot's signature** (a scheme-only URL needs
no slash, so the id survives sanitising). Only a plain id — letters, digits,
`.`, `_`, `:`, `-` — becomes a label; anything else is dropped and the link
keeps the fixed label `this run`. The URL itself is safe either way:
`deeplink.Run` path-escapes the id.

**An engine that predates the member.** C153 is a *warning*, so a bundle
declaring `{{run.url}}` still admits on an older runner, where the reference
arrives unsubstituted. A value still carrying a `{{` delimiter is treated as
no address (the shape `bots/review-pr`'s `ai_value` has always used), so the
footer degrades to the id alone instead of publishing a dead relative link.
That is why this costs no `requires.iterion` bump: refusing to run a malware
scan because a signature line cannot render would be the worse trade.

## The chat lane — both halves carry it

`bots/prod-watch` (Argus) ends every Mattermost message — an alert, a notice,
the digest, a session message — with the footer, and `bots/feed-watch` (Vigie)
now ends its digest with it too. Neither is a copy of the forge footer: the
chat text goes through `clip(text, max_chars)`, so a footer appended after the
clip can push the message past the webhook's limit, and one appended before it
is the first thing cut. Each half reserves the footer's room FIRST, then fits
the body into what is left:

- **prod-watch** computes `body_chars = max_message_chars - len(footer) - 1`
  (the labels' check, a note's parts and the clip all work in it), then clips —
  `TestProdWatch_AClippedMessageStillEndsWithItsRun` (e2e) sends a message long
  enough to be cut and reads the link at its end.
- **feed-watch** delivers through its deterministic `notify` node and builds
  the footer there: the run link, and beside it the Revi-style provenance
  — `Étape | Modèle | Harness | Effort demandé | Tokens` for the synthesize
  step. Mattermost renders no `<details>` fold
  ([mattermost#38480](https://github.com/mattermost/mattermost/issues/38480)),
  so the block travels unfolded at the bottom of the LAST message, and the
  split reserves its room: the tail part is re-split — never cut — so body,
  marker and footer all fit one budget (the `max_messages` ceiling, which
  already drops content and fails the run, re-trims the capped tail instead).
  A silence alert carries the signature alone: no LLM step stands behind it.

The chat-lane guard (`bots/chat_run_link_test.go`) pins the class the way the
forge one does, so a chat-facing bot that joins must carry the link and cannot
drop out quietly. Both halves of
[#2430](https://github.com/SocialGouv/iterion/issues/2430) are covered.
## The guard

`bots/forge_run_link_test.go` discovers the class from the bundles — a `.bot`
naming `forge_publish_url`, or a bundle shipping a `skills/forge-*.md` — and
pins its membership, so a bundle that *joins* must carry the link and one that
*leaves* reddens instead of dropping out of every check. The channel is read
from the **compiled** workflow (a prompt's refs, a tool body's refs): a
`{{run.url}}` sitting in a `##` comment is not a channel, and a guard that
accepted one would be green over a bot publishing nothing.
