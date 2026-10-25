import { describe, expect, it } from "vitest";

import { buttonsWithDivDescendants, iconsWithLabelMissingRole } from "./_jsxTree";

// Content-model guards for the a11y-chokepoints stream. Each pins an
// a11y class the tree already satisfies, so a regression is a red test
// instead of silent drift:
//   - RGAA 8.2 / HTML5 content model: a <button> accepts phrasing
//     content ONLY — an intrinsic <div> descendant is invalid markup.
//     Keep the block shape with a display:flex/block <span>.
//   - RGAA 1.1: an icon carrying an aria-label WITHOUT role="img" —
//     a bare Radix <svg> exposes neither role nor name, so a lone
//     aria-label is dropped by assistive tech. Route labelled icons
//     through ui/Icon, which pairs the name with the role (and makes
//     unlabelled ones decorative).
describe("jsx content model", () => {
  it("renders no <div> inside a <button> — phrasing content only (RGAA 8.2)", () => {
    const hits = buttonsWithDivDescendants();
    if (hits.length) {
      throw new Error(
        `a <button> contains a <div> (invalid HTML5 content model) — use a display:flex/block <span> instead:\n${hits
          .map((h) => `${h.path}:${h.line}`)
          .join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("pairs every labelled icon with role=img — a bare Radix <svg> drops the name (RGAA 1.1)", () => {
    const hits = iconsWithLabelMissingRole();
    if (hits.length) {
      throw new Error(
        `an *Icon element carries aria-label without role="img" — render it through ui/Icon:\n${hits
          .map((h) => `${h.path}:${h.line} <${h.tag}>`)
          .join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });
});
