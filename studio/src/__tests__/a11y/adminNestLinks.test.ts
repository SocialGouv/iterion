import { describe, expect, it } from "vitest";

import { absoluteTargetHits } from "./_adminNestLinks";

// The DOMAIN of source-discipline's "no absolute path literal under
// views/admin" guard. That guard runs absoluteTargetHits over the real tree,
// which proves it accepts the tree as it is; it cannot prove what the detector
// REFUSES. These cases run the same function over hand-written console modules
// — one per spelling an absolute target reaches wouter through, each a line
// that would resolve under /admin/admin/… in the app.
//
// A console module here is a string, and this file is a test file, so the real
// scan (which skips /__tests__/ and *.test.*) never sees these paths.

const MODULE = "/src/views/admin/ProbePage.tsx";

/** Hits for one hand-written console module. */
function hits(src: string, path = MODULE): string[] {
  return absoluteTargetHits([[path, src]]);
}

describe("absoluteTargetHits — refuses an absolute console target", () => {
  it("flags the plain href spelling", () => {
    expect(hits(`return <Link href="/admin/users">U</Link>;`)).toHaveLength(1);
  });

  it("flags a target assembled from a literal", () => {
    expect(hits(`navigate("/orgs/" + org.id);`)).toHaveLength(1);
  });

  it("flags a literal in a table consumed elsewhere", () => {
    expect(hits(`const TABS = [{ value: "/admin/dlq", label: "DLQ" }];`)).toHaveLength(1);
  });

  // The two bypasses a per-line comment strip allows: `//` and `/*` inside a
  // string literal are string content, not the start of a comment, and a
  // blanker that does not know strings swallows the href that follows.
  it("flags an href whose line carries a URL in a string before it", () => {
    expect(
      hits(`return <Link title="docs at https://iterion.dev" href="/admin/users">U</Link>;`),
    ).toHaveLength(1);
  });

  it("flags an href after a string that contains a block-comment opener", () => {
    const src = ['const GLOB = "/*";', 'const TARGET = "/admin/dlq";', 'const END = "*/";'].join(
      "\n",
    );
    expect(hits(src)).toEqual([expect.stringContaining("ProbePage.tsx:2")]);
  });

  // studioRoute() is the blessed way out of the nest, so its own argument is
  // exempt — the REST of the line is not.
  it("flags a second target on a line that also calls studioRoute()", () => {
    expect(
      hits(`return <Link href={e ? studioRoute("/orgs/o1") : "/admin/users"}>U</Link>;`),
    ).toHaveLength(1);
  });

  // A computed target spells its first segment as an interpolation, so the
  // literal starts `` `/${ `` — absolute all the same, and the shape the
  // console's own cross-links take.
  it("flags a first segment that is interpolated", () => {
    expect(hits("return <Link href={`/${kind}/${id}`}>x</Link>;")).toHaveLength(1);
  });

  it("flags an interpolated target sharing a line with studioRoute()", () => {
    expect(
      hits("const to = e ? studioRoute(`/orgs/${id}`) : `/${kind}/${id}`;"),
    ).toHaveLength(1);
  });
});

describe("absoluteTargetHits — accepts what the console legitimately writes", () => {
  it("accepts a relative table path", () => {
    expect(hits(`return <Link href={item.path}>{item.label}</Link>;`)).toEqual([]);
  });

  it("accepts a target handed to studioRoute()", () => {
    expect(hits(`navigate(studioRoute("/orgs/" + org.id));`)).toEqual([]);
  });

  it("accepts a quoted path in prose", () => {
    expect(hits(`// a literal "/admin/users" would resolve under /admin/admin/.`)).toEqual([]);
    expect(hits(`/* "/admin/users" is what this used to be. */`)).toEqual([]);
  });

  it("accepts a bare slash separator", () => {
    expect(hits(`const key = parts.join("/");`)).toEqual([]);
  });

  it("accepts an absolute URL in a string", () => {
    expect(hits(`const DOC = "https://iterion.dev/admin/users";`)).toEqual([]);
  });

  it("accepts the route tables, the licensed homes of a console path", () => {
    const src = `export const ADMIN_BASE = "/admin";`;
    expect(hits(src, "/src/views/admin/adminRoutes.ts")).toEqual([]);
    expect(hits(src, "/src/views/admin/AdminConsoleRoutes.tsx")).toEqual([]);
  });

  it("ignores a module outside the console", () => {
    expect(hits(`return <Link href="/admin/users">U</Link>;`, "/src/views/Home.tsx")).toEqual([]);
  });
});

// What a text scan cannot see, executed rather than claimed — the render tests
// (AdminNav, UserDrawer, AuditTab, consoleScreens) are what hold this half.
describe("absoluteTargetHits — the limits it has, spelled out", () => {
  it("still sees a target whose first segment survives the split", () => {
    expect(hits(['navigate("/ad" +', '  "min/users");'].join("\n"))).toHaveLength(1);
  });

  it("does not see a target whose leading slash is split off", () => {
    expect(hits(['navigate("/" +', '  "admin/users");'].join("\n"))).toEqual([]);
  });

  // The same shape on one line, and deliberately: a lone "/" literal is the
  // separator a relative path is joined with in every other place the studio
  // writes it, so refusing it would refuse the join.
  it("does not see a leading slash written as its own literal", () => {
    expect(hits(`navigate("/" + kind + "/" + org.id);`)).toEqual([]);
  });

  it("flags rather than hides when a line's quotes do not balance", () => {
    expect(hits(`const re = /['"]/; // href="/admin/users"`)).toHaveLength(1);
  });
});
