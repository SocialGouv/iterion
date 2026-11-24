import { describe, expect, it } from "vitest";

import { prohibitedNames, prohibitedNamesIn } from "./_genericNames";
import { files } from "./_scanner";

// The source guard #2767 asks for: an accessible name on an element whose
// implicit role is generic is prohibited (ARIA in HTML, axe
// aria-prohibited-attr), and the rendered axe net only sees the surfaces
// somebody mounted — this scan reads the whole tree. A fix earns the
// element a role or turns the name into text; a waiver names the test that
// pins the site, never a shrug.
const WAIVED: { path: string; tag: string; reason: string }[] = [];

// One scan for the whole file: parsing every .tsx costs ~250 ms, and both
// guards below judge the same immutable list.
const ALL = prohibitedNames();

const hits = (src: string) => prohibitedNamesIn("/src/x.tsx", src).map((h) => `${h.tag}:${h.attr}`);

describe("a11y / generic naming scan — the shapes it must read", () => {
  it("flags a name on a bare span, div or p", () => {
    expect(hits(`export const X = () => <span aria-label="Unread" />;`)).toEqual(["span:aria-label"]);
    expect(hits(`export const X = () => <div aria-label="Loading board">…</div>;`)).toEqual(["div:aria-label"]);
    expect(hits(`export const X = () => <p aria-labelledby="h" />;`)).toEqual(["p:aria-labelledby"]);
  });

  it("exempts a generic element that declares a naming role", () => {
    expect(hits(`export const X = () => <span role="img" aria-label="drag" />;`)).toEqual([]);
    expect(hits(`export const X = () => <div role="region" aria-label="Shares">…</div>;`)).toEqual([]);
  });

  it("takes a runtime role for declared — undecidable here", () => {
    expect(hits(`export const X = ({ r }: { r: string }) => <span role={r} aria-label="x" />;`)).toEqual([]);
  });

  it("still flags a role literal that prohibits naming", () => {
    expect(hits(`export const X = () => <span role="generic" aria-label="x" />;`)).toEqual(["span:aria-label"]);
    expect(hits(`export const X = () => <div role="presentation" aria-label="x" />;`)).toEqual(["div:aria-label"]);
    expect(hits(`export const X = () => <p role="none" aria-label="x" />;`)).toEqual(["p:aria-label"]);
  });

  it("reads a runtime name as readily as a literal one", () => {
    expect(hits(`export const X = ({ n }: { n: number }) => <span aria-label={\`\${n} unread\`}>{n}</span>;`)).toEqual(["span:aria-label"]);
  });

  it("leaves components and named elements alone", () => {
    expect(hits(`export const X = () => <Button aria-label="Close" />;`)).toEqual([]);
    expect(hits(`export const X = () => <button aria-label="Close" />;`)).toEqual([]);
    expect(hits(`export const X = () => <section aria-label="Shares" />;`)).toEqual([]);
  });
});

describe("no accessible name on a roleless element (ARIA in HTML)", () => {
  it("leaves no generic element carrying a name without a role", () => {
    const unwaived = ALL.filter(
      (h) => !WAIVED.some((w) => w.path === h.path && w.tag === h.tag),
    );
    if (unwaived.length) {
      throw new Error(
        `aria-label / aria-labelledby on an element whose implicit role is generic (give it a role, or make the name text):\n${unwaived
          .map((h) => `  ${h.path}:${h.line}  <${h.tag}${h.role ? ` role="${h.role}"` : ""} ${h.attr}=…>`)
          .join("\n")}`,
      );
    }
    expect(unwaived).toHaveLength(0);
  });

  it("is not vacuous — the tree holds generics whose roles EARN their name", () => {
    // The role="img" glyphs and role="group"/"status" blocks are the legal
    // shape: if the reader stopped seeing them, the guard above would pass
    // on an unread tree.
    const src = files
      .filter(([path]) => path.endsWith(".tsx"))
      .map(([, s]) => s)
      .join("\n");
    const legal = src.match(/<(span|div|p)[^>]*role="(img|group|region|status|progressbar)"/g) ?? [];
    expect(legal.length).toBeGreaterThanOrEqual(8);
  });

  // A waiver is a live claim about a site. One that matches nothing is a
  // sentence nobody can check, and the entry it hides behind is how a list
  // like this one stops describing the tree it guards.
  it("carries no waiver the tree no longer needs", () => {
    const dead = WAIVED.filter(
      (w) => !ALL.some((h) => h.path === w.path && h.tag === w.tag),
    );
    expect(dead, "waivers that match nothing — the element is fixed, drop them").toHaveLength(0);
  });
});
