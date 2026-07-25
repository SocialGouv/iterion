# Iterion Studio

React + Vite SPA for the Iterion engine. The same application serves local
workflow authoring/runs and the cloud control plane: visual editor, run console,
session and pipeline boards, bot catalogue/launch forms, triggers and schedules,
dispatcher, marketplace/plugins/skills/secrets, forge integrations, team/org
administration, usage/audit views, and scoped configuration editors.

Build and check it from the repository root:

```bash
devbox run -- task studio:check   # lint + typecheck + tests
devbox run -- task studio:dev     # watch backend + Vite dev frontend
devbox run -- task studio:build   # production bundle (embedded by pkg/server)
```

Contributor references live in [`docs/`](docs/):
[design-system.md](docs/design-system.md) (tokens, UI primitives,
adoption discipline) and [visual-identity.md](docs/visual-identity.md).
Product/user documentation lives in the repository-level
[`docs/`](../docs/README.md), especially [visual-editor.md](../docs/visual-editor.md)
and [cloud.md](../docs/cloud.md).

## Data fetching

New data fetching uses
[@tanstack/react-query](https://tanstack.com/query) (`useQuery` /
`useQueryClient`). The app-wide client is configured in
[`src/main.tsx`](src/main.tsx) — staleTime 0, retry 1, no window-focus
refetch. House style:

- Array query keys, kebab-case scope first, params after:
  `["run-commits", runId]`, `["forge-connections", teamID]`. Reuse an
  existing key when fetching the same resource so caches stay shared.
- Surface errors as strings via `errorMessage(query.error)`
  ([`src/lib/errorHints.ts`](src/lib/errorHints.ts)), rendered in an
  `InlineBanner`.
- Refresh via `queryClient.invalidateQueries({ queryKey })` for
  event-driven invalidation, or `query.refetch()` for an
  operator-triggered reload of one query.
- Gate fetching with `enabled:`, or by mounting the component only
  while its data is on screen (dialogs/drawers).

Manual `useState` + `useEffect` fetch triads are legacy — don't add new
ones; migrate opportunistically when touching a file, provided the
migration preserves behavior (loading states, error surfaces, refetch
triggers). Leave effects that do more than fetch-and-set (store writes,
derived form seeding) alone until they can be restructured deliberately.
