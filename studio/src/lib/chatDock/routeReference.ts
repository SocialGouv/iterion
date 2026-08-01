// Where the operator is, expressed in the typed-reference vocabulary.
//
// The assistant gets context two ways and they must speak the same
// language, or a bot has to handle two protocols:
//   explicit — you dropped something into the dock (the drop chips)
//   implicit — you are looking at it right now (this file)
//
// A reference is a POINTER, never inlined content: `run/019f…`, not the
// run's events. The assistant resolves it through the tools it already
// has (__mcp-control for runs, __mcp-board for cards), so a big page
// costs the prompt nothing and the assistant reads only what it needs.
//
// The map below is declarative on purpose. Adding a route means adding a
// row here — not a `useChatContext()` call inside another view, which is
// how this kind of thing rots (half the views wired, nobody sure which).

export type ReferenceKind =
  | "run"
  // node/<run>/<node> — a single node of a run. Not reachable from the
  // URL today (node selection is store state), so it arrives only as an
  // explicit drop. Part of the vocabulary so both paths agree on it.
  | "node"
  | "card"
  | "bot"
  | "repo"
  // A view with no single entity behind it. Still worth reporting: "I am
  // looking at the board" is real context even without a card selected.
  | "view";

export interface TypedReference {
  kind: ReferenceKind;
  // Canonical wire form — `<kind>/<id>`. This is what the assistant is
  // told, and what an explicit drop chip produces for the same thing.
  ref: string;
  // Short human label for the pinned chip. The operator must be able to
  // read what the assistant is assumed to be looking at.
  label: string;
}

function ref(kind: ReferenceKind, id: string, label: string): TypedReference {
  return { kind, ref: `${kind}/${id}`, label };
}

// Run ids are long; the chip shows a recognisable head.
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

function basename(path: string): string {
  const parts = path.split("/").filter(Boolean);
  return parts.length > 0 ? parts[parts.length - 1] : path;
}

type RouteBuilder = (
  params: Record<string, string>,
  search: URLSearchParams,
) => TypedReference | null;

interface RouteRule {
  // Path pattern. A ":name" segment captures; a trailing "*" matches
  // every remaining segment (used for the /admin family).
  path: string;
  build: TypedReference | RouteBuilder | null;
}

// First match wins, like wouter's <Switch> — so the literal routes sit
// above their ":param" siblings, same ordering rule as App.tsx.
const ROUTE_RULES: readonly RouteRule[] = [
  // The assistant's own route: you are already in the conversation, so a
  // chip saying "you are looking at the conversation" is noise.
  { path: "/whats-next", build: null },
  { path: "/", build: null },

  { path: "/runs/new", build: ref("view", "launch", "Launch") },
  {
    path: "/runs/:id",
    build: (p) => ref("run", p.id, `Run ${shortId(p.id)}`),
  },
  { path: "/runs", build: ref("view", "runs", "Runs") },

  {
    path: "/pipelines/cards/:kind/:id",
    build: (p) =>
      p.kind === "run"
        ? ref("run", p.id, `Run ${shortId(p.id)}`)
        : ref("card", p.id, `Card ${shortId(p.id)}`),
  },
  { path: "/pipelines", build: ref("view", "pipelines", "Pipelines") },

  { path: "/board/labels", build: ref("view", "board-labels", "Board labels") },
  { path: "/board/fields", build: ref("view", "board-fields", "Board fields") },
  { path: "/board", build: ref("view", "board", "Board") },

  { path: "/bots/new", build: ref("view", "bot-builder", "Bot builder") },
  { path: "/bots/:name", build: (p) => ref("bot", p.name, p.name) },
  { path: "/bots", build: ref("view", "bots", "Bots") },

  // The editor addresses a workspace file via ?file=; bare /editor is
  // the picker, which points at nothing in particular.
  {
    path: "/editor",
    build: (_p, search) => {
      const file = search.get("file");
      if (!file) return ref("view", "editor", "Editor");
      return ref("bot", file, basename(file));
    },
  },

  { path: "/repos/:key", build: (p) => ref("repo", p.key, p.key) },

  { path: "/skills", build: ref("view", "skills", "Skills") },
  { path: "/integrations/connect", build: ref("view", "integrations", "Connect repository") },
  { path: "/integrations/bind", build: ref("view", "integrations", "Bind bot") },
  { path: "/integrations", build: ref("view", "integrations", "Integrations") },
  { path: "/insights", build: ref("view", "insights", "Insights") },
  { path: "/dispatcher", build: ref("view", "dispatcher", "Dispatcher") },
  { path: "/triggers", build: ref("view", "automations", "Automations") },
  { path: "/marketplace", build: ref("view", "marketplace", "Marketplace") },
  { path: "/plugins", build: ref("view", "plugins", "Plugins") },
  { path: "/secrets", build: ref("view", "secrets", "Secrets") },
  { path: "/config-editor", build: ref("view", "config-editor", "Config editor") },
  { path: "/account", build: ref("view", "account", "Account") },
  { path: "/teams/:id", build: ref("view", "teams", "Team") },
  { path: "/orgs/:id", build: ref("view", "orgs", "Organisation") },
  // The wildcard also matches bare /admin (nothing left to consume), so
  // the family needs one row, not two.
  { path: "/admin/*", build: ref("view", "admin", "Admin") },
];

// The route that renders the assistant full-width. The dock stands down
// there: two composers over one session on the same screen is the exact
// ambiguity this feature is supposed to remove, and the draft is shared
// (both write the same run store's chatDraft), so the second one would
// also be an echo.
export const ASSISTANT_ROUTE = "/whats-next";

export function isAssistantOwnRoute(path: string): boolean {
  return path === ASSISTANT_ROUTE;
}

// matchPath returns the captured params, or null when the pattern
// doesn't apply. Segment-wise so "/board" never matches "/board/labels".
export function matchPath(
  pattern: string,
  path: string,
): Record<string, string> | null {
  const want = pattern.split("/").filter(Boolean);
  const got = path.split("/").filter(Boolean);
  const wildcard = want[want.length - 1] === "*";
  if (wildcard) {
    if (got.length < want.length - 1) return null;
  } else if (want.length !== got.length) {
    return null;
  }
  const params: Record<string, string> = {};
  for (let i = 0; i < want.length; i++) {
    const seg = want[i];
    if (seg === "*") break;
    if (seg.startsWith(":")) {
      params[seg.slice(1)] = safeDecode(got[i]);
      continue;
    }
    if (seg !== got[i]) return null;
  }
  return params;
}

// A path segment can carry a stray "%" that decodeURIComponent rejects;
// a malformed URL must degrade to "no context", never throw into the
// render.
function safeDecode(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

/**
 * referenceForRoute maps a location to the thing the operator is looking
 * at. Returns null when the route points at nothing in particular (home,
 * the assistant's own route) or is unknown — an unmapped route must
 * yield NO context rather than a wrong one.
 *
 * @param path   pathname, as wouter's useLocation() reports it
 * @param search raw query string, as wouter's useSearch() reports it
 */
export function referenceForRoute(
  path: string,
  search = "",
): TypedReference | null {
  const params = new URLSearchParams(search);
  for (const rule of ROUTE_RULES) {
    const captured = matchPath(rule.path, path);
    if (!captured) continue;
    if (rule.build === null) return null;
    if (typeof rule.build === "function") return rule.build(captured, params);
    return rule.build;
  }
  return null;
}
