// The super-admin console's route table — the one place a console screen is
// declared. App.tsx mounts one <Route> per entry and AdminNav renders the same
// entries as its sections, so a screen that exists is reachable and highlighted
// by construction (App.tsx's page map is keyed by AdminPath, so adding an entry
// here without a page, or a page without an entry, fails to compile).
//
// Paths are RELATIVE to the console's own mount point. App.tsx mounts the group
// with `<Route path="/admin" nest>`, and inside a wouter nest both useLocation()
// and navigate() work against that base: "/users" — never "/admin/users", which
// would resolve to /admin/admin/users. A link written from OUTSIDE the console
// (the sidebar, the home hub) is resolved against the studio base instead and
// goes through adminPath().

export interface AdminNavItem {
  readonly path: string;
  readonly label: string;
}

export interface AdminNavSection {
  readonly id: string;
  readonly label: string;
  readonly items: readonly AdminNavItem[];
}

/** Where the console is mounted, relative to the studio base. */
export const ADMIN_BASE = "/admin";

export const ADMIN_SECTIONS = [
  {
    id: "tenancy",
    label: "Tenancy",
    items: [
      { path: "/orgs", label: "Organizations" },
      { path: "/users", label: "Users" },
    ],
  },
  {
    id: "platform",
    label: "Platform",
    items: [
      { path: "/llm-credentials", label: "LLM credentials" },
      { path: "/forge-apps", label: "Forge OAuth apps" },
      { path: "/settings/usage-caps", label: "Usage caps" },
      { path: "/settings/bot-roles", label: "Bot roles" },
      { path: "/settings/sandbox", label: "Sandbox" },
      { path: "/settings/bot-vars", label: "Bot vars" },
      { path: "/settings/platform-credentials", label: "Platform creds" },
      { path: "/bots", label: "Bot overrides" },
    ],
  },
  {
    id: "ops",
    label: "Ops",
    items: [
      { path: "/audit", label: "Audit" },
      { path: "/dlq", label: "Dead-letter queue" },
      { path: "/spend", label: "Spend" },
      { path: "/runs", label: "Runs" },
    ],
  },
] as const satisfies readonly AdminNavSection[];

/** Every console path the table declares. */
export type AdminPath = (typeof ADMIN_SECTIONS)[number]["items"][number]["path"];

export interface AdminItem {
  readonly path: AdminPath;
  readonly label: string;
}

/** Every entry, flat and in nav order. */
export const ADMIN_ITEMS: readonly AdminItem[] = ADMIN_SECTIONS.flatMap(
  (section): readonly AdminItem[] => section.items,
);

export const ADMIN_PATHS: readonly AdminPath[] = ADMIN_ITEMS.map((item) => item.path);

/** The console home: bare /admin, and any console path the table no longer
 *  declares, redirect here. */
export const ADMIN_HOME: AdminPath = "/orgs";

/** adminPath spells a console path for a link written OUTSIDE the console,
 *  where the router base is the studio's and not the console's. */
export function adminPath(path: AdminPath): string {
  return ADMIN_BASE + path;
}

/**
 * consolePath is the table path a nested-router location designates — what
 * anything comparing `useLocation()` against the table must compare. It folds
 * exactly the two spellings the route matcher accepts beyond the table's own,
 * each of which renders a screen the nav then has to credit:
 *
 *   - ONE trailing slash — the server redirects `/admin/users/` to
 *     `/studio/admin/users/` (registerStudioLegacyRedirects: a trailing slash
 *     is a published address) and wouter reports that nest location as
 *     "/users/", which the route "/users" still matches. Only one: "/users//"
 *     matches no route, so it is left alone rather than credited to an entry
 *     the operator is not on.
 *   - case — wouter compiles route patterns with the `i` flag and compares the
 *     base case-insensitively, so /admin/USERS renders the users console. Every
 *     table path is lowercase, which is what makes folding safe here; a table
 *     path with a case-sensitive segment would need its own comparison.
 */
export function consolePath(location: string): string {
  const withoutSlash =
    location.length > 1 && location.endsWith("/") ? location.slice(0, -1) : location;
  return withoutSlash.toLowerCase();
}
