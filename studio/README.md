# Iterion Studio

React + Vite SPA for the iterion engine: workflow editor canvas, run
console, board, dispatcher dashboard. Built and checked from the repo
root:

```bash
devbox run -- task studio:check   # lint + typecheck + tests
devbox run -- task studio:dev     # watch backend + Vite dev frontend
devbox run -- task studio:build   # production bundle (embedded by pkg/server)
```

Contributor references live in [`docs/`](docs/):
[design-system.md](docs/design-system.md) (tokens, UI primitives,
adoption discipline) and [visual-identity.md](docs/visual-identity.md).

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
