# Diagrams

Entity-relationship models of the MongoDB data and the access-control matrix, drawn from the code.

Every diagram page here is a **view** over the code, never a second source of truth: each one names the commit it was verified against, links the Go files it was drawn from, and can be refused by `internal/docsguard` when the tree moves away from it (a collection without an entity, an entity without a collection, a gate its file no longer declares). The prose pages of [`docs/`](../README.md) keep the explanations; these pages keep the shapes.

## Pages

| Page | What it draws |
|---|---|
| [data-model-identity.md](data-model-identity.md) | Orgs, teams, users and their memberships; invitations; the credentials a person holds (sessions, PATs, reset tokens, SSO state); org-level SSO config; the audit trail. |
| [data-model-forge-bots.md](data-model-forge-bots.md) | A forge connection, the repositories it binds, the webhooks it provisions and the bots it enables — the worked example *Revi bound to a repository, inside a team*; the inbound delivery lifecycle; the bot-definition tiers (team, platform, baked); the board binding and the provisioning gate. |
| [data-model-runs.md](data-model-runs.md) | A run and its per-run streams (events, logs, turns, plans, notes, interactions), the run tree (parents, sub-bots, forks) and delegations, assistant supervision; the run status machine. |
| [data-model-credentials.md](data-model-credentials.md) | Provider keys and forfaits at rest, the sealed bundle and the pool lease in flight, the four meters and the routing policy; the credential tier walk. |
| [data-model-automation.md](data-model-automation.md) | The trigger spine (subscriptions, effects, board events and cursors), the two scheduled surfaces, and what is leased by whom. |
| [data-model-knowledge.md](data-model-knowledge.md) | Memory spaces and documents, config shares and their deliveries, user notifications. |
| [access-control.md](access-control.md) | Who can be a caller (12 principal kinds), the roles, the authorization decision points, and the gate × principal matrix. |
| [flow-auth-login.md](flow-auth-login.md) | Flow — signing in and authenticating a request. |
| [flow-org-team-lifecycle.md](flow-org-team-lifecycle.md) | Flow — org and team lifecycle: suspend, soft-delete, purge. |
| [flow-forge-connect-enable-bot.md](flow-forge-connect-enable-bot.md) | Flow — connect a forge, enable a bot on a repository. |
| [flow-webhook-to-run.md](flow-webhook-to-run.md) | Flow — webhook-to-run: the admission gates, every refusal, and the one chokepoint. |
| [flow-revi-review-merge-gate.md](flow-revi-review-merge-gate.md) | Flow — The revi/review merge gate. |
| [flow-cloud-run-lifecycle.md](flow-cloud-run-lifecycle.md) | Flow — Cloud run lifecycle. |
| [flow-credential-resolution.md](flow-credential-resolution.md) | Flow — Credential resolution — which credential funds a cloud run. |
| [flow-secrets-sealed-bundle.md](flow-secrets-sealed-bundle.md) | Flow — the sealed credentials bundle. |
| [flow-config-share.md](flow-config-share.md) | Flow — config-share: the mint, the two access paths, the fail-closed write. |

Reading order for a newcomer: identity → forge & bots → runs → access control; credentials, automation and knowledge stand alone.

## Conventions

### Every page

- The first paragraph is one plain sentence with no link: it is the page's row in the [generated docs map](../references/map-docs.md).
- A blockquote names the commit the page was verified against. When the guard refuses a page, the fix is to redraw it and bump the commit, never to loosen the guard. What it reads is listed under *How the pages are kept true*; a change of behaviour passes it, so bumping the commit means re-verifying the page against the code.
- Symbols are named in the text; links point at files, never at line numbers (they rot within days).
- **Sources** lists the Go files the page was drawn from, as repository-relative links. **Related** lists the prose pages that explain the same domain.
- Present tense only: a page says what the code does, not what it used to do.

### Entity-relationship pages (`data-model-*.md`)

- One `erDiagram` block per ten entities at most, each under a *Block* heading followed by reading notes — the things a reader must not infer wrongly from the picture.
- An entity is a Mongo collection, named exactly as the code names it. The only other entities are **marked** participants: `"S3: …"` for blob storage, `"FS: …"` for a filesystem-backed participant, `"NATS KV: …"` for a key-value bucket, `"external: …"` for a system outside iterion.
- The key line is written per document, because `_id` is not one thing here: a plain id (`string _id PK`, or `ObjectId _id PK` when no writer sets it), the tenant itself (`string _id PK, FK "teams._id - the tenant IS the id"`), a name (`"name"`), a composite (`"{tenant_id, run_id}"`, `"org|<id>|<month>"`), or a document family (`"family: bot_roles|runner_pools|…"` when one collection holds documents of several shapes, each field naming in its comment the family that holds it).
- A tenant or owner attribute carries its **value set** in the comment: `"team id | orgtier:<org> | platform:"`. `tenant_id` does not always mean a team — the comment says which (team, org, the org's primary team). A value set may leave out what `DisableAuth` (a development mode) writes — a tenant that reads `dev` ([data-model-runs.md](data-model-runs.md)) or is empty; some name it.
- A relationship label opens with the field that links the two entities, or with *no field* when none does. When only one end of the line has a crow's foot (`{` or `}`), the entity at that end holds the field unless the label opens with its holder's name (`retry_circuits.key`); with crow's feet at both ends or at neither, the label opens with the holder's name, or only one of the two entities draws the field. A dotted line (`..`) is a link not to be read as a plain key — such as a slug or a name, an id inside a composed string, or no field at all — and its label or its block's notes say what it is.
- Mongo enforces no reference, so a delete that does not cascade can leave any reference dangling: an `||` end says the writers always set the reference, not that its target is still there.
- Below the blocks, the **Collections** table has one row per entity: owner package and document struct, the tenant field and its values, the unique and TTL indexes, and the **twin** — what persists the same thing when iterion runs without Mongo (for example `file <name>`, `memory-only`, `S3`, `NATS KV`, `none (cloud-only)`). A collection another page charts in full may stub its row with a link there.

### Flow pages (`flow-*.md`)

- `flowchart` for branching paths, `sequenceDiagram` for exchanges between actors, `stateDiagram-v2` for lifecycles.
- A state diagram opens with `%% vocabulary: <Go type>` on its own line, because several vocabularies look alike and are not interchangeable (see the glossary). It holds no `;` outside a `%%` comment line: after a label's colon — a transition's, a state description's, a note's — mermaid ends the statement at the `;` and draws the rest of the line as stray states, and `check-mermaid.mjs` lets nearly every such line through, so the docs guard refuses any `;` on a state-block line that is not a `%%` comment, in the fences it reads (see *How the pages are kept true*). Use a dash or a comma. This holds for every state diagram of the pages in `docs/diagrams/`, the `data-model-runs.md` status machine included.
- Guards that refuse are drawn, not omitted; the collections a flow touches are named; a step table pairs each step with the code that takes it.
- One feature, one diagram: where a prose page already owns the diagram of a feature, the flow page links it instead of redrawing it.

### Mermaid, as this repository's parser accepts it

In `erDiagram` blocks, labels and comments are quoted and never contain a double quote; keys are written `PK, FK` with the comma; attribute types are single tokens; `%%` comments stand on their own line; a sentinel such as `platform:` lives inside a quoted comment, never as a bare entity name; `direction` and entity aliases are not used in `erDiagram`. A block opens with ```` ```mermaid ```` and nothing after the word, on a page with LF line endings: that is the form `docs/scripts/check-mermaid.mjs` finds and parses before the site builds.

## Legend

| Notation | Meaning |
|---|---|
| `||` · `|o` or `o|` · `}o` or `o{` | at that end of the line: exactly one · zero or one · zero or more |
| `A }o--|| B : "field"` | `field` links A and B, and A — the only end with a crow's foot — holds it; a label opening `B.field` says B does |
| `--` · `..` | solid · dotted: a link not to be read as a plain key, which its label or its block's notes explain |
| `PK` · `FK` · `UK` | the document id · a reference to a collection · a field under a unique index |
| `"S3: …"` `"FS: …"` `"NATS KV: …"` `"external: …"` | a participant that is not a Mongo collection |

## Glossary

- **Team, tenant, org.** A *team* is the resource tenant: almost every tenant-scoped collection keys on `teams._id`, and `tenant_id` usually means a team id. An *org* owns teams, the member roster, SSO and the monthly budgets; a few collections key on the org instead, and a few org-level settings are keyed on the org's *primary team*. Sentinel tenants exist: `platform:` for deployment-wide records, `orgtier:<org>` for org-tier credentials.
- **Bot identifiers.** A bundle's *name* is its technical id (`bots/<name>/manifest.yaml`); its *display name* is the persona (Revi, Billy, …); a team-authored bot is a `bot_sources` row with its own `_id` and a *slug* unique per team. A repository binding stores slugs; at launch the slug resolves *team bot source → platform override → baked catalog*.
- **State vocabularies.** Several look alike; for example `store.RunStatus` (a run: queued, running, paused_waiting_human, paused_operator, finished, failed, failed_resumable, cancelled), the native board state (a card's column, by default inbox, backlog, ready, waiting_deps, in_progress, awaiting_input, review, done, blocked), the dispatcher claim lifecycle (unclaimed, claimed, retry scheduled), the forge board columns (`forge_board_bindings.status_mapping` maps them to native states), and `BankState` (banked, bank_failed). A diagram names the one it draws.
- **Access control, permissions, scopes.** This directory's *access control* is iterion's own RBAC (roles, gates). [permissions.md](../permissions.md) is about what a workflow node may do (allow / ask / deny); [forge-permissions.md](../forge-permissions.md) is about the scopes of a forge token.

## How the pages are kept true

- `go test ./internal/docsguard/` — holds the pages' inventory to the tree: every collection a `.Collection(…)` call site in `pkg/`'s production Go files resolves to, and every name the guard's dynamic-site table declares, must be an entity of an ER page, and every entity a collection or a marked participant; every function holding a dynamic `.Collection(<expr>)` call must be a row of the dynamic-site table (a row names a function by file and name), and every row such a function; every element of the org purger's two swept lists must be a string literal naming a collection one of those call sites names; every gate symbol in the guard's table must still be declared in the file its row names and be named in the access-control page's source. On the data-model pages it also refuses an erDiagram line in none of the forms it reads (an entity block, a relationship in the symbol cardinalities, a bare entity), and on every page of the directory a `;` on a state-diagram line outside a `%%` comment. It reads mermaid as these pages write it and is no parser of mermaid or markdown: a line it takes for one form that mermaid reads as another goes by unrefused, and a fence inside an HTML block or a four-column indent is read although the site draws nothing there. In Go it checks shadowing only for an identifier argument inside a function declaration: a selector's base or a package-level site that a local name shadows is read as the constant it is spelled like.
- `node docs/scripts/check-mermaid.mjs` — every Mermaid block in that form parses.
- `task map:check` — the generated docs map lists every page.
- `task docs:links` — every link resolves.
