# ADR-080 — Git-backed, org-private plugin sources

Status: **accepted** (2026-07-20).

## Context

[ADR-079](079-cloud-portable-plugin-and-library-skills.md) made an enabled
plugin's skills reach a cloud **runner** pod. It resolves them on the
**launching** instance, which is the only place that can see the operator's
iterion home.

That left two holes, both discovered while wiring a real org-private deploy
plugin onto the prod instance:

1. **The server pod's home is ephemeral too.** Installing a plugin there (via
   the studio, or by hand) works until the next restart, then vanishes. The
   failure is **silent**: mirroring is best-effort by design, so runs simply
   proceed without the skill. A deploy bot that loses its platform playbook
   still runs — it just does the wrong thing and reports success. We hit
   exactly this: the plugin had to be re-injected into all three replicas after
   every rollout.
2. **There is no org scoping.** Plugin enablement is a single global
   `plugins.yaml` (or `ITERION_PLUGINS_ENABLE`) per instance. A multi-tenant
   cloud instance cannot give team A its own private plugin without giving it
   to everyone.

The chart offers no seam either: `server-deployment.yaml` has no
`extraVolumes`/`initContainers` hook, so a ConfigMap mount would require a chart
release — and would still be instance-global, not per-team.

## Decision

**Move the authority off the pod's filesystem: a team-scoped record says which
git repository holds the plugin, and the checkout is only ever a re-derivable
cache.**

`pluginsource.PluginSource` = `(tenant_id, name, git_url, ref, secret_id,
enabled)`, persisted in Mongo (collection `plugin_sources`) — the same durable
substrate the rest of cloud state uses. `(tenant_id, name)` is unique **per
team**, never globally, so two orgs may each bring their own `deploy-target`.

- **Fetch + cache** (`pluginsource.Fetcher`) keyed by `(git_url, ref)`. With a
  **pinned** ref the checkout is immutable, so every launch after the first
  costs no network. This is why the design prefers pinning to a moving branch:
  it collapses "resolve at launch" into a no-network operation *without*
  introducing the staleness window a refresh worker would (see Alternatives).
  A moving ref is allowed but warns.
- **Credential by reference.** `secret_id` points at a `GenericSecret` (PAT or
  deploy key). The value is injected through a `0700` askpass helper — never
  argv, never the URL — and redacted from command output. Same discipline as the
  mounted run secrets.
- **Resolution at launch** (`cloudpublisher.resolveContributionsFor`) for the
  team that owns the **run**, merged into the ADR-079 payload, so the transport
  to the runner is already solved. A locally installed plugin of the same
  `(kind, name)` shadows the git-hosted one **in place**: one name must resolve
  to exactly one payload entry, or the runner's mirror order would silently pick
  the winner.
- **Failure is loud.** Unlike the local registry's best-effort skip, a source
  the operator explicitly enabled that fails to fetch **fails the launch**.
  Silently contributing nothing is the precise failure this ADR exists to
  remove.
- **REST** at `/api/teams/{id}/plugin-sources`, team admin/owner only — a source
  designates code mirrored into *every* run of the team, so it is org automation
  policy, not a personal preference. Responses carry `pinned_ref` so the UI can
  surface the drift risk.

Validation rejects local filesystem paths: a tenant must not be able to point a
source at the server's own disk.

## Consequences

- An org-private plugin is now **durable** (survives restarts, re-derived from
  Mongo) and **genuinely org-scoped** — the two properties ADR-079 could not
  provide on its own. Together they make "attach a swappable deploy-target to a
  bot, privately, on a shared cloud instance" actually work.
- Updating a plugin becomes an **explicit, auditable act** (bump the ref) rather
  than a silent drift, when the ref is pinned.
- The pod's iterion home is no longer load-bearing for team plugins. Instance-
  wide plugins (builtins, `ITERION_PLUGINS_ENABLE`) are unchanged.
- **Not covered yet**: a studio UI for sources (REST only), per-**bot** binding
  (a source applies to all of the team's runs), and plugin `hooks` — still
  unported since ADR-079, since they merge into `settings.json` rather than
  mirroring markdown.

## Alternatives rejected

- **A refresh worker** populating a local cache, with launches reading the
  cache. Optimises a latency we have not measured, and pays for it with a
  **staleness window** — the one failure mode that is genuinely dangerous here,
  since a stale deploy-target deploys *wrongly* without erroring. It also does
  not remove the synchronous path: a cold pod's cache is empty, so the
  launch-time resolve has to exist anyway. Pinned refs give the same
  no-network steady state with none of that.
- **Chart `extraVolumes` + a ConfigMap.** Needs a chart release, and remains
  instance-global — it would fix persistence but not org scoping.
- **Baking the plugin into the iterion image.** Freezes the plugin set at build
  time and publishes an org's private playbook in a public image.
