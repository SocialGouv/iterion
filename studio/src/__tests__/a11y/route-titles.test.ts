import ts from "typescript";
import { describe, expect, it } from "vitest";

import { ROUTE_TITLES, routeTitleFor } from "@/lib/routeTitles";
import { files } from "./_scanner";

// RGAA 8.6 (#2516): every page's title must be relevant — name THAT page.
// The rendered net saw 15 routes sharing one identical title because the
// hook named only the routes an if/else branch remembered. The title now
// comes from the route table (lib/routeTitles.ts), and this guard holds the
// table in step with App.tsx's <Route path="…"> declarations in BOTH
// directions, so a new view cannot ship untitled and a removed route cannot
// strand its entry.

// Paths outside useDocumentTitle's scope: the pre-auth and self
// -authenticating doors never render StudioShell, so the hook never runs
// for them and they keep the index.html title. An entry here is a claim
// about WHY the route needs no table title — the dead-entry check below
// keeps the list honest.
const PUBLIC_ROUTES: { path: string; reason: string }[] = [
  { path: "/login", reason: "pre-auth sign-in page — StudioShell is not mounted" },
  { path: "/auth/password/change", reason: "password door — authenticates on the URL token" },
  { path: "/auth/forgot-password", reason: "password door — authenticates on the URL token" },
  { path: "/auth/reset", reason: "password door — authenticates on the URL token" },
  { path: "/invitations/accept", reason: "pre-auth invitation door" },
  { path: "/config/:id", reason: "shell-less config-share editor — self-authenticating" },
];

// Table entries with no <Route> literal in App.tsx: the route exists, but a
// view mounts it internally rather than the top-level Switch.
const NON_LITERAL: { pattern: string; reason: string }[] = [
  { pattern: "/pipelines/cards/:kind/:id", reason: "the pipelines view routes its card pages internally" },
];

/** Every string-literal `path` attribute on a <Route> in App.tsx. */
function routeLiterals(): string[] {
  const entry = files.find(([path]) => path === "/src/App.tsx");
  if (!entry) throw new Error("the scanner did not read /src/App.tsx");
  const [path, src] = entry;
  const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const out: string[] = [];
  const visit = (n: ts.Node): void => {
    if (
      (ts.isJsxOpeningElement(n) || ts.isJsxSelfClosingElement(n)) &&
      n.tagName.getText() === "Route"
    ) {
      for (const a of n.attributes.properties) {
        if (ts.isJsxAttribute(a) && a.name.getText() === "path" && a.initializer && ts.isStringLiteral(a.initializer)) {
          out.push(a.initializer.text);
        }
      }
    }
    ts.forEachChild(n, visit);
  };
  visit(sf);
  return [...new Set(out)];
}

const LITERALS = routeLiterals();
const PATTERNS = ROUTE_TITLES.map((e) => e.pattern);

describe("route titles — the table covers every declared route (RGAA 8.6)", () => {
  it("names every route App.tsx declares", () => {
    const untitled = LITERALS.filter(
      (p) => !PATTERNS.includes(p) && !PUBLIC_ROUTES.some((x) => x.path === p),
    );
    if (untitled.length) {
      throw new Error(
        `routes with no title in lib/routeTitles.ts (a page must name itself — add an entry, or a PUBLIC_ROUTES reason):\n${untitled
          .map((p) => `  ${p}`)
          .join("\n")}`,
      );
    }
    expect(untitled).toHaveLength(0);
  });

  it("strands no table entry on a route App.tsx no longer declares", () => {
    const dead = PATTERNS.filter(
      (p) => !LITERALS.includes(p) && !NON_LITERAL.some((x) => x.pattern === p),
    );
    if (dead.length) {
      throw new Error(
        `route-title entries no route declares (the route is gone — drop them, or a NON_LITERAL reason):\n${dead
          .map((p) => `  ${p}`)
          .join("\n")}`,
      );
    }
    expect(dead).toHaveLength(0);
  });

  it("carries no exclusion the tree no longer needs", () => {
    const deadPublic = PUBLIC_ROUTES.filter((x) => !LITERALS.includes(x.path));
    const deadNonLiteral = NON_LITERAL.filter((x) => !PATTERNS.includes(x.pattern));
    expect(deadPublic, "PUBLIC_ROUTES entries matching no declared route").toHaveLength(0);
    expect(deadNonLiteral, "NON_LITERAL entries matching no table entry").toHaveLength(0);
  });

  it("never gives two pages the same static title", () => {
    const titles = ROUTE_TITLES.flatMap((e) => (e.title ? [e.title] : []));
    expect(new Set(titles).size).toBe(titles.length);
  });

  it("resolves a distinct title for every route the audit found identical", () => {
    // The 15 routes of the RGAA 8.6 constat (the audit's kebab-case page ids
    // spelled back as paths) — each must now name its page, distinctly.
    const paths = [
      "/board/fields",
      "/board/labels",
      "/board",
      "/bots/featurly",
      "/bots/new",
      "/bots",
      "/dispatcher",
      "/",
      "/insights",
      "/marketplace",
      "/plugins",
      "/secrets",
      "/skills",
      "/triggers",
      "/whats-next",
    ];
    expect(paths).toHaveLength(15);
    const contexts = paths.map((p) => {
      const entry = routeTitleFor(p);
      if (!entry) throw new Error(`${p}: no route-title entry`);
      // A derived entry's distinctness is the URL's own (its :param).
      return entry.title ?? entry.pattern;
    });
    expect(new Set(contexts).size).toBe(paths.length);
  });
});
