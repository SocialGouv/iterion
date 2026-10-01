# The ENGINE stays bot-agnostic

Read it when engine code is about to name a bot or bake one in.

## The ENGINE stays bot-agnostic — no bot knowledge in `pkg/`/`cmd/`

The mirror of "catalog bots are repo/stack-agnostic": **iterion the engine
must never know about a SPECIFIC catalog bot.** A bot is a catalog artifact
(`bots/<name>/`); the engine wires *any* bot through GENERIC seams and must
carry no `"docs-refresh"` / `"branch-improve-loop"` / `"review-pr"` string,
no `stampDocsRefreshAmendVars`-style helper, no bot-specific prompt, no
`if botID == "<x>"` branch. That coupling is exactly backwards — it makes
the engine un-shippable to anyone whose bots differ, and it means a new bot
needs an engine PR instead of just a bundle.

**When a bot needs special launch/runtime behaviour, the behaviour lives in
the BOT, keyed on generic context the engine already provides:**
- Generic launch vars every bot can read — `pr_url`, `base_ref`,
  `source_branch`, `pr_author`, `scope_notes`, … — set uniformly for ANY bot
  launched on a PR/issue (`reviewPRVars` / `buildPRForgeCommandVars`). Doki's
  amend-on-PR (v3.5.2) is the reference: iterion checks out the PR head +
  sets `pr_url`/`base_ref` for whatever bot the webhook launches; Doki *itself*
  reads a non-empty `pr_url` and switches into amend — zero engine code knows
  it's Doki.
- Manifest `invocations:` (the capability "what can fire me"), `capabilities:`
  (board tools), `contributes:` (plugins), skills. The `Subscription` binds
  (event) → (a bot) generically.
- Manifest **`produces:` / `consumes:`** — the run-to-run hand-off, matched by
  KIND (`review`, `review_ledger`), never by bot id. A bot declares what it
  leaves behind for a later run (naming nodes in its OWN graph) and what it
  wants stamped into a launch var; the engine knows the shape of each role and
  nothing about who fills it. This is how a reviewer seeds a fixer, and how the
  fixer's per-finding answer reaches the next review, with neither manifest
  naming the other bot. Adding a second reviewer or a second fixer is a bundle,
  not an engine PR. See [pkg/server/webhooks_handoff.go](../../../pkg/server/webhooks_handoff.go).

**Known debt (extract when touched, don't extend):** the webhook role bot
ids are no longer read as constants — they resolve through
`Server.roleBots()` over the `bot_roles` platform-settings family
([pkg/platformcfg](../../../pkg/platformcfg/platformcfg.go), `iterion remote admin
roles set --reviewer …`), the constants remaining only as the DEFAULTS
(enforced by the symbol-sweep test in
[bot_resolver_sweep_test.go](../../../pkg/server/bot_resolver_sweep_test.go)). What
remains hardcoded: the Billy merge-queue auto-heal mission prompt
([pkg/server/webhooks_github.go](../../../pkg/server/webhooks_github.go)), the
`botRosterOrder` display list ([pkg/server/server_dsl.go](../../../pkg/server/server_dsl.go)),
and the dispatcher's `ImplementBotOrDefault → "feature-dev"`
([pkg/dispatcher/config.go](../../../pkg/dispatcher/config.go), local-YAML
configurable already). Full role-from-manifest extraction stays future
work. **Do not add to this list** — thread new behaviour through the
generic seams above. If you find a fresh instance, flag it.

