// The studio's route titles — the source of truth useDocumentTitle reads,
// one entry per page (RGAA 8.6, #2516). Before this table the hook's if/else
// chain named 8 routes and let every other page fall through to the bare
// project name, so 15 pages of the audit sample shared one identical title.
//
// A route whose title is DERIVED from live state (the open run's name, the
// active editor tab, the URL's own :param) is computed by the hook; its
// entry still exists so the guard sees the route, and a static `title` on
// such an entry is the fallback read while the state loads.
//
// The a11y guard (__tests__/a11y/route-titles.test.ts) holds this table in
// step with App.tsx's <Route path="…"> declarations in both directions, so a
// new view cannot ship untitled and a removed route cannot strand its title.
//
// Matching is first-match-wins in DECLARATION ORDER, mirroring the Switch in
// App.tsx: the literal /bots/new must sit above /bots/:name, and /runs/new
// above /runs/:id.

export interface RouteTitle {
  /** The path pattern as App.tsx writes it (":param" segments allowed). */
  pattern: string;
  /** Static page title; undefined when the hook derives it from live state. */
  title?: string;
}

export const ROUTE_TITLES: readonly RouteTitle[] = [
  { pattern: "/", title: "Home" },
  { pattern: "/whats-next", title: "What's Next" },
  // Derived: the active editor tab's file or label.
  { pattern: "/editor" },
  { pattern: "/runs/new", title: "Launch" },
  // Derived: the run's own name once its header loads.
  { pattern: "/runs/:id", title: "Run" },
  { pattern: "/runs", title: "Runs" },
  { pattern: "/bots/new", title: "New bot" },
  // Derived: the catalog name is in the URL itself.
  { pattern: "/bots/:name" },
  { pattern: "/bots", title: "Bots" },
  { pattern: "/board", title: "Board" },
  { pattern: "/board/labels", title: "Board labels" },
  { pattern: "/board/fields", title: "Board fields" },
  { pattern: "/pipelines", title: "Pipelines" },
  // Routed inside the pipelines view, not by an App.tsx <Route> literal.
  { pattern: "/pipelines/cards/:kind/:id", title: "Pipeline card" },
  { pattern: "/dispatcher", title: "Dispatcher" },
  { pattern: "/insights", title: "Insights" },
  // The nav names this view "Automations"; the URL kept the old name.
  { pattern: "/triggers", title: "Automations" },
  { pattern: "/marketplace", title: "Marketplace" },
  { pattern: "/plugins", title: "Plugins" },
  { pattern: "/skills", title: "Skills" },
  { pattern: "/secrets", title: "Secrets" },
  { pattern: "/integrations", title: "Integrations" },
  { pattern: "/integrations/connect", title: "Connect a repository" },
  { pattern: "/integrations/bind", title: "Bind a bot" },
  { pattern: "/account", title: "Account" },
  { pattern: "/orgs/:id", title: "Organization" },
  { pattern: "/teams/:id", title: "Team" },
  { pattern: "/repos/:key", title: "Repository" },
  { pattern: "/sitemap", title: "Sitemap" },
  { pattern: "/config-editor", title: "Config editor" },
];

/** The one entry whose pattern matches this location, in declaration order. */
export function routeTitleFor(location: string): RouteTitle | undefined {
  const path = location.split("?")[0]!.split("#")[0]!;
  return ROUTE_TITLES.find((entry) => matches(entry.pattern, path));
}

/** wouter's exact-segment match: a ":param" segment reads one path segment. */
function matches(pattern: string, path: string): boolean {
  const want = pattern.split("/").filter(Boolean);
  const got = path.split("/").filter(Boolean);
  if (want.length !== got.length) return false;
  return want.every((seg, i) => seg.startsWith(":") || seg === got[i]);
}
