# Architecture decision records

One decision per file, named `NNN-kebab-title.md`, where `NNN` is the next
free number. The number is the record's **identifier**: it is cited from code
comments, commit messages, pull requests and other ADRs, and from places this
repository cannot reach — a chat thread, a ticket on another forge, someone's
notes. That is why a number, once published, is never reassigned and never
renumbered: doing so repairs the links inside this tree by silently breaking
every citation outside it.

## Numbers used twice

Eight numbers were nevertheless taken twice, by decisions written in parallel.
They are listed here so an ambiguous `ADR-NNN` stays **resolvable**: a reader
who follows one of these numbers reaches this table, and picks the subject
they meant. `TestADRNumberCollisionsAreDeclared` keeps the table honest in
both directions — a new collision that is not listed fails, and a listed
number that no longer collides fails too.

**Do not add a row to this table to make a build pass.** A new record takes a
free number; this table only records what was already published.

| Number | Record | Subject |
|---|---|---|
| `002` | [002-desktop-assetserver-proxy.md](002-desktop-assetserver-proxy.md) | Desktop studio hosted via the Wails AssetServer handler |
| `002` | [002-editor-runview-separation.md](002-editor-runview-separation.md) | Separating the Run console from the Workflow editor |
| `075` | [075-irref-fallback-for-oversized-cloud-queue-ir.md](075-irref-fallback-for-oversized-cloud-queue-ir.md) | IRRef fallback: offloading an oversized compiled IR at cloud dispatch |
| `075` | [075-nexie-roadmap-study-cycle-skill-level.md](075-nexie-roadmap-study-cycle-skill-level.md) | Nexie v3: the roadmap-study cycle as a skill-level behaviour |
| `076` | [076-pipeline-hard-blockers-and-waiting-deps.md](076-pipeline-hard-blockers-and-waiting-deps.md) | Pipeline hard blockers, `waiting_deps`, unified launch admission |
| `076` | [076-scoped-config-share-editor.md](076-scoped-config-share-editor.md) | Scoped config-share editor (self-service, per-field, per-repo) |
| `090` | [090-model-registry-and-operator-model-choice.md](090-model-registry-and-operator-model-choice.md) | A model registry, and letting the operator choose the model |
| `090` | [090-runtime-operational-settings-db-backed.md](090-runtime-operational-settings-db-backed.md) | Runtime-configurable operational settings, DB-backed |
| `091` | [091-fallback-skip-route-and-plan-peer-review.md](091-fallback-skip-route-and-plan-peer-review.md) | `action: skip` terminal route, `when:` route gate, cross-model plan phase |
| `091` | [091-ubiquitous-assistant-chat-dock.md](091-ubiquitous-assistant-chat-dock.md) | The assistant is a shell-level dock, not a route |
| `093` | [093-bot-vars-runtime-settings.md](093-bot-vars-runtime-settings.md) | Bot-var overrides resolved from the DB before the pod env |
| `093` | [093-model-spec-registry-as-a-leaf-package.md](093-model-spec-registry-as-a-leaf-package.md) | The model-spec registry is a leaf package; its prices are a cost tier |
| `094` | [094-shared-subbot-bundles-bot-uri.md](094-shared-subbot-bundles-bot-uri.md) | Shared subbot bundles: the `bot://` dependency URI |
| `094` | [094-trigger-effect-outbox.md](094-trigger-effect-outbox.md) | Durable effect outbox for cloud board triggers |
| `098` | [098-connector-catalog.md](098-connector-catalog.md) | The connector catalog: MCP for agents, deterministic nodes for workflows |
| `098` | [098-dsl-versioning-and-authoring-surface.md](098-dsl-versioning-and-authoring-surface.md) | DSL versioning, explicit imports, and the authoring surfaces |

Citing one of these: name the file, not the number —
`docs/adr/098-connector-catalog.md` rather than `ADR-098`.
