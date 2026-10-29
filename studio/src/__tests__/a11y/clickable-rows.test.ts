import { describe, expect, it } from "vitest";

import { scanRows, where, type RowScan } from "./_clickableRows";

// The rules of the clickable-row scan (#2766), exercised on sources written
// for the purpose: each rule is seen firing AND seen staying quiet on the
// shape it must not flag, which a tree-level assertion alone cannot show
// (a scan that returns nothing is green on an empty tree too).
//
// source-discipline.test.ts is where the same scan judges the real tree.

const ROW = (body: string, imports = ""): [string, string][] => [
  [
    "/src/surface.tsx",
    `import { clickableRowProps, pointerRowProps } from "@/lib/a11y";\n${imports}\nexport function Surface() {\n  return (\n${body}\n  );\n}\n`,
  ],
];

const scan = (files: [string, string][]): RowScan => scanRows(files);
const lines = (hits: RowScan[keyof RowScan]): string => hits.map(where).join("\n");

describe("clickable-row scan / a row that IS the control holds nothing focusable", () => {
  it("names a row holding an intrinsic control", () => {
    const s = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <button type="button">Resume</button>
    </div>`),
    );
    expect(s.nested, lines(s.nested)).toHaveLength(1);
    expect(s.nested[0]!.detail).toContain("<button>");
    expect(s.mouseOnly).toHaveLength(0);
  });

  it("stays quiet on a row holding only text and layout", () => {
    const s = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <span className="truncate">{label}</span>
      <div><p>two lines</p></div>
    </div>`),
    );
    expect(s.nested, lines(s.nested)).toHaveLength(0);
    expect(s.sites).toHaveLength(1);
  });

  it("follows a COMPONENT descendant to its own definition", () => {
    // The case that decides this scan: at most call sites the focusable
    // descendant is a component, and a scan keyed on intrinsic tag names
    // reports a clean tree while the rows it is meant to judge are broken.
    const files: [string, string][] = [
      ...ROW(
        `    <div {...clickableRowProps(open)}>
      <BotAvatar run={run} />
    </div>`,
        `import { BotAvatar } from "./BotAvatar";`,
      ),
      [
        "/src/BotAvatar.tsx",
        `export function BotAvatar() {\n  return <button type="button">bot</button>;\n}\n`,
      ],
    ];
    const s = scan(files);
    expect(s.nested, lines(s.nested)).toHaveLength(1);
    expect(s.nested[0]!.detail).toContain("<BotAvatar>");
    expect(s.unresolved, lines(s.unresolved)).toHaveLength(0);
  });

  it("credits a component with ITS OWN markup, not the whole file's", () => {
    const files: [string, string][] = [
      ...ROW(
        `    <div {...clickableRowProps(open)}>
      <Badge>done</Badge>
    </div>`,
        `import { Badge } from "./ui";`,
      ),
      [
        "/src/ui.tsx",
        `export function Badge({ children }) {\n  return <span>{children}</span>;\n}\n` +
          `export function Dismissable() {\n  return <button type="button">x</button>;\n}\n`,
      ],
    ];
    const s = scan(files);
    expect(s.nested, lines(s.nested)).toHaveLength(0);
  });

  it("reads a component the call site made a tab stop, whatever it renders", () => {
    const files: [string, string][] = [
      ...ROW(
        `    <div {...clickableRowProps(open)}>
      <Plain tabIndex={0} />
    </div>`,
        `import { Plain } from "./ui";`,
      ),
      ["/src/ui.tsx", `export function Plain() {\n  return <span>x</span>;\n}\n`],
    ];
    const s = scan(files);
    expect(s.nested, lines(s.nested)).toHaveLength(1);
    expect(s.nested[0]!.detail).toContain("tabIndex");
  });

  it("reads tabIndex for what it is: 0 is a tab stop, -1 is not", () => {
    const stop = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <div tabIndex={0}>focus me</div>
    </div>`),
    );
    expect(stop.nested, lines(stop.nested)).toHaveLength(1);
    const notAStop = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <div tabIndex={-1}>programmatic only</div>
    </div>`),
    );
    expect(notAStop.nested, lines(notAStop.nested)).toHaveLength(0);
  });

  it("judges a file input by its type, never by a utility class", () => {
    // `sr-only` keeps a control in the tree and focusable — it is why the
    // drop zones were nested-interactive — while `type="hidden"` is the one
    // file input that is not focusable at all.
    const srOnly = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <input type="file" className="sr-only" aria-label="Upload" />
    </div>`),
    );
    expect(srOnly.nested, lines(srOnly.nested)).toHaveLength(1);
    const hidden = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <input type="hidden" value="1" />
    </div>`),
    );
    expect(hidden.nested, lines(hidden.nested)).toHaveLength(0);
  });

  it("counts a link as a control only once it can be followed", () => {
    const withHref = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <a href="/runs">runs</a>
    </div>`),
    );
    expect(withHref.nested, lines(withHref.nested)).toHaveLength(1);
    const anchorOnly = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <a id="top">anchor</a>
    </div>`),
    );
    expect(anchorOnly.nested, lines(anchorOnly.nested)).toHaveLength(0);
  });

  it("names a row nested inside another row", () => {
    const s = scan(
      ROW(`    <div {...clickableRowProps(open)}>
      <div {...clickableRowProps(inner)}>inner</div>
    </div>`),
    );
    expect(s.sites).toHaveLength(2);
    expect(s.nested, lines(s.nested)).toHaveLength(1);
  });
});

describe("clickable-row scan / a pointer row needs a control inside it", () => {
  it("names a pointer row with nothing focusable — that row is mouse-only", () => {
    const s = scan(
      ROW(`    <div {...pointerRowProps(open)}>
      <span>{label}</span>
    </div>`),
    );
    expect(s.mouseOnly, lines(s.mouseOnly)).toHaveLength(1);
    expect(s.nested).toHaveLength(0);
  });

  it("stays quiet once the row holds the keyboard path", () => {
    const s = scan(
      ROW(`    <div {...pointerRowProps(open)}>
      <button type="button">Open</button>
    </div>`),
    );
    expect(s.mouseOnly, lines(s.mouseOnly)).toHaveLength(0);
  });
});

describe("clickable-row scan / nothing shadows the helper's own props", () => {
  it("names the row that writes its own onClick beside the spread", () => {
    const s = scan(
      ROW(`    <div {...clickableRowProps(open)} onClick={(e) => open(e)}>
      <span>{label}</span>
    </div>`),
    );
    expect(s.shadowed, lines(s.shadowed)).toHaveLength(1);
    expect(s.shadowed[0]!.detail).toContain("onClick");
  });

  it("names role, tabIndex and onKeyDown too", () => {
    const s = scan(
      ROW(`    <div {...pointerRowProps(open)} role="button" tabIndex={0} onKeyDown={k}>
      <button type="button">Open</button>
    </div>`),
    );
    expect(s.shadowed, lines(s.shadowed)).toHaveLength(1);
    for (const prop of ["role", "tabIndex", "onKeyDown"]) {
      expect(s.shadowed[0]!.detail).toContain(prop);
    }
  });

  it("leaves a row that only adds its own unrelated props alone", () => {
    const s = scan(
      ROW(`    <div {...clickableRowProps(open)} className="row" title={hint} draggable>
      <span>{label}</span>
    </div>`),
    );
    expect(s.shadowed, lines(s.shadowed)).toHaveLength(0);
  });
});

describe("clickable-row scan / what it cannot follow, it reports", () => {
  it("reports a descendant from an external package rather than clearing it", () => {
    const s = scan(
      ROW(
        `    <div {...clickableRowProps(open)}>
      <Handle type="target" />
    </div>`,
        `import { Handle } from "@xyflow/react";`,
      ),
    );
    expect(s.unresolved, lines(s.unresolved)).toHaveLength(1);
    expect(s.unresolved[0]!.detail).toContain("<Handle>");
    // And it is NOT silently counted as a control either: the guard's
    // waiver list is where that call gets made.
    expect(s.nested).toHaveLength(0);
  });

  it("reports a self-closing row instead of calling it empty", () => {
    const s = scan(
      ROW(
        `    <Card {...pointerRowProps(open)} />`,
        `import { Card } from "./ui";`,
      ).concat([["/src/ui.tsx", `export function Card() {\n  return <div>c</div>;\n}\n`]]),
    );
    expect(s.sites).toHaveLength(1);
    expect(s.unresolved, lines(s.unresolved)).toHaveLength(1);
    expect(s.unresolved[0]!.detail).toContain("self-closing");
    // Neither rule may fire on a row whose content the scan cannot see.
    expect(s.mouseOnly).toHaveLength(0);
    expect(s.nested).toHaveLength(0);
  });

  it("reads the row component's own markup, and reports one it cannot read", () => {
    const resolved: [string, string][] = [
      ...ROW(
        `    <Row {...clickableRowProps(open)}>
      <span>{label}</span>
    </Row>`,
        `import { Row } from "./ui";`,
      ),
      [
        "/src/ui.tsx",
        `export function Row({ children }) {\n  return <tr><td>{children}</td><td><button type="button">x</button></td></tr>;\n}\n`,
      ],
    ];
    const s = scan(resolved);
    expect(s.nested, lines(s.nested)).toHaveLength(1);
    expect(s.nested[0]!.detail).toContain("own markup");

    const external = scan(
      ROW(
        `    <Row {...clickableRowProps(open)}>
      <span>{label}</span>
    </Row>`,
        `import { Row } from "some-table-lib";`,
      ),
    );
    expect(external.unresolved, lines(external.unresolved)).toHaveLength(1);
    expect(external.unresolved[0]!.detail).toContain("itself");
  });
});
