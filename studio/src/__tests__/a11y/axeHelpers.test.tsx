// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import axe from "axe-core";
import { afterEach, describe, expect, it } from "vitest";

import { allFiles } from "./_scanner";
// Read as TEXT, a second way: Vite's glob excludes the module that declares
// it (the scanner) and this file is the glob's own consumer, so the two files
// most likely to host a laxer net are the two the scan cannot reach.
import scannerSource from "./_scanner.ts?raw";
import selfSource from "./axeHelpers.test.tsx?raw";
import {
  AXE_BEST_PRACTICE_RULES,
  AXE_WCAG_TAGS,
  axeRunOptions,
  expectNoViolationsStrict,
  expectUniqueAccessibleNames,
} from "./axeHelpers";

afterEach(cleanup);

// The one net exists because axe "incomplete" (needs review) was silently
// dropped: the dangling-aria-labelledby hole the AdminNav entry conceded.
// Leg 1 of #2591: an incomplete fails unless the surface waives the rule by
// name. This file is the guard of the guard — it judges the helper, which is
// what every other axe entry in the studio trusts.
describe("expectNoViolationsStrict — incompletes are never dropped", () => {
  it("fails on an axe-incomplete dangling aria-labelledby", async () => {
    const { container } = render(
      <section aria-labelledby="does-not-exist">
        <button type="button">ok</button>
      </section>,
    );
    await expect(expectNoViolationsStrict(container, "dangling")).rejects.toThrow(
      /needs review/,
    );
    await expect(expectNoViolationsStrict(container, "dangling")).rejects.toThrow(
      /aria-valid-attr-value/,
    );
  });

  it("passes when the surface waives the rule by name, and only that rule", async () => {
    const { container } = render(
      <section aria-labelledby="does-not-exist">
        <button type="button">ok</button>
      </section>,
    );
    await expect(
      expectNoViolationsStrict(container, "waived", [
        {
          rule: "aria-valid-attr-value",
          node: "aria-labelledby=does-not-exist",
          reason: "pinned by AdminNav.test.tsx",
        },
      ]),
    ).resolves.toBeUndefined();
  });

  it("names the offending node, not just the rule — a log is the whole fix path", async () => {
    const { container } = render(
      <section aria-labelledby="does-not-exist">
        <button type="button">ok</button>
      </section>,
    );
    await expect(expectNoViolationsStrict(container, "dangling")).rejects.toThrow(
      /aria-labelledby="does-not-exist"/,
    );
  });
});

// #2591's other half: the rule set the net runs is DECLARED, so widening it
// is one edit and so nothing silently joins or leaves it.
describe("the declared rule scope", () => {
  it("RUNS every rule it declares — a declared rule is not an executed one", async () => {
    // The trap this closes: axe skips an `experimental` rule, and one that is
    // off by default, even when its tag is in runOnly. Asked of the registry
    // (getRules), `target-size` and `focus-order-semantics` look in scope;
    // asked of a real run under the net's own options, they were in no bucket
    // at all. So the oracle is the run.
    const { container } = render(
      <main>
        <h1>surface</h1>
        <span tabIndex={0}>focusable, no widget role</span>
        <button type="button">ok</button>
      </main>,
    );
    const results = await axe.run(container, axeRunOptions());
    const ran = new Set(
      [
        ...results.violations,
        ...results.incomplete,
        ...results.passes,
        ...results.inapplicable,
      ].map((r) => r.id),
    );
    const declared = new Set([
      ...axe.getRules([...AXE_WCAG_TAGS]).map((r) => r.ruleId),
      ...AXE_BEST_PRACTICE_RULES,
    ]);
    // `bypass` needs the document root (a means to skip repeated blocks),
    // which a mounted fragment has no answer for — the same blind spot as
    // the page-level landmark family, and the only one.
    const inert = [...declared].filter((id) => !ran.has(id) && id !== "bypass");
    expect(inert).toEqual([]);
    expect(ran.size).toBeGreaterThan(70);
    // Both halves of the declaration, named: the WCAG 2.2 rule axe ships
    // disabled, and the experimental best-practice one.
    expect([...AXE_WCAG_TAGS]).toContain("wcag22aa");
    expect(ran).toContain("target-size");
    expect(ran).toContain("focus-order-semantics");
  });

  it("catches what focus-order-semantics is declared for", async () => {
    // The rule was inert for a release and nobody noticed, because nothing
    // exercised what it judges. This is that something.
    const { container } = render(
      <div>
        <span tabIndex={0}>focusable, no widget role</span>
      </div>,
    );
    await expect(
      expectNoViolationsStrict(container, "roleless focusable"),
    ).rejects.toThrow(/focus-order-semantics/);
  });

  it("runs the declared best-practice rules (a positive tabindex fails)", async () => {
    expect([...AXE_BEST_PRACTICE_RULES]).toContain("tabindex");
    const { container } = render(
      <div>
        <span tabIndex={3}>jumps the queue</span>
      </div>,
    );
    await expect(expectNoViolationsStrict(container, "tabindex")).rejects.toThrow(
      /\[tabindex\]/,
    );
  });

  it("runs only the declared ones — an undeclared best-practice rule stays off", async () => {
    expect([...AXE_BEST_PRACTICE_RULES]).not.toContain("accesskeys");
    const { container } = render(
      <div>
        <button type="button" accessKey="s">
          save
        </button>
        <button type="button" accessKey="s">
          send
        </button>
      </div>,
    );
    // Non-vacuous by construction: the same fixture DOES break `accesskeys`
    // when the rule is asked for, so the helper passing it is the allowlist
    // at work and not a fixture that happens to be clean.
    const direct = await axe.run(container, {
      runOnly: { type: "rule", values: ["accesskeys"] },
    });
    expect(direct.violations.map((v) => v.id)).toContain("accesskeys");
    await expect(
      expectNoViolationsStrict(container, "undeclared"),
    ).resolves.toBeUndefined();
  });

  it("declares the best-practice half as an allowlist — axe's other rules are off", () => {
    const off = (axeRunOptions().rules ?? {}) as Record<
      string,
      { enabled: boolean }
    >;
    const bestPractice = axe.getRules(["best-practice"]).map((r) => r.ruleId);
    const wcagScoped = new Set(
      axe.getRules([...AXE_WCAG_TAGS]).map((r) => r.ruleId),
    );
    for (const rule of bestPractice) {
      const declared =
        AXE_BEST_PRACTICE_RULES.includes(rule as never) || wcagScoped.has(rule);
      // The WCAG half decides for a rule carrying both: switching it off
      // here would shrink that half.
      expect(off[rule]).toEqual({ enabled: declared });
    }
    // The registry has to have been read for any of this to mean anything.
    expect(bestPractice.length).toBeGreaterThan(20);
  });
});

// A library's internal sentinel is not a surface's defect — but the drop is
// per node, so the surface's own mistake under the same rule still fails.
describe("vendor sentinels", () => {
  it("drops Radix's focus guard (aria-hidden + tabindex=0 by design)", async () => {
    const { container } = render(
      <div>
        <span
          data-radix-focus-guard=""
          aria-hidden="true"
          tabIndex={0}
          style={{ outline: "none", opacity: 0, pointerEvents: "none" }}
        />
        <button type="button">ok</button>
      </div>,
    );
    await expect(
      expectNoViolationsStrict(container, "radix guard"),
    ).resolves.toBeUndefined();
  });

  it("keeps the surface's own aria-hidden focusable failing", async () => {
    const { container } = render(
      <div>
        <div aria-hidden="true">
          <button type="button">unreachable, still in the tab order</button>
        </div>
      </div>,
    );
    await expect(
      expectNoViolationsStrict(container, "ours"),
    ).rejects.toThrow(/aria-hidden-focus/);
  });

  it("reads the sentinel's own tag even when an attribute holds a '>'", async () => {
    // openingTag() stops at the first UNQUOTED '>': a title="a > b" before
    // the marker would otherwise hide the guard and turn every open-dialog
    // audit red on a node the surface does not own.
    const { container } = render(
      <div>
        <span
          title="a > b"
          data-radix-focus-guard=""
          aria-hidden="true"
          tabIndex={0}
          style={{ outline: "none", opacity: 0, pointerEvents: "none" }}
        />
        <button type="button">ok</button>
      </div>,
    );
    await expect(
      expectNoViolationsStrict(container, "radix guard, angled title"),
    ).resolves.toBeUndefined();
  });

  it("still recognises the guard when axe truncates its markup", async () => {
    // axe rewrites `html` past 300 characters, cutting every attribute name
    // and value to 20 — `data-radix-focus-guard` becomes
    // `data-radix-focus-gua...`. A sentinel matched in that string stops
    // matching the day the library adds an attribute; matched on the
    // element, it does not.
    const { container } = render(
      <div>
        <span
          data-radix-focus-guard=""
          aria-hidden="true"
          tabIndex={0}
          title={"a > b " + "x".repeat(200)}
          data-filler={"y".repeat(200)}
          style={{ outline: "none", opacity: 0, pointerEvents: "none" }}
        />
        <button type="button">ok</button>
      </div>,
    );
    await expect(
      expectNoViolationsStrict(container, "radix guard, truncated"),
    ).resolves.toBeUndefined();
  });

  it("still fails when the surface's own node CONTAINS a sentinel", async () => {
    // The hostile shape, not the cooperative one: axe reports a node by its
    // outerHTML, so a wrapper that merely contains the guard must not be
    // mistaken for the guard. This is the real layout of the three
    // document.body audits (Drawer, ChatDockShell, pipeline card drawer).
    const { container } = render(
      <div>
        <div aria-hidden="true">
          <span
            data-radix-focus-guard=""
            tabIndex={0}
            style={{ outline: "none", opacity: 0, pointerEvents: "none" }}
          />
          <button type="button">unreachable, still in the tab order</button>
        </div>
      </div>,
    );
    await expect(
      expectNoViolationsStrict(container, "ours, around a guard"),
    ).rejects.toThrow(/aria-hidden-focus/);
  });
});

// A waiver is the one lane a surface has for "this rule is wrong about this
// pattern, and here is what judges it instead". It has to work on whichever
// bucket axe files the rule under, and it has to die when it stops being
// true.
describe("waivers", () => {
  it("covers a violation, not only a needs-review", async () => {
    // focus-order-semantics is a VIOLATION on the window-splitter pattern
    // (role=separator + aria-valuenow IS a widget in ARIA 1.2, axe's role
    // list predates it) — the shape chokepoints.test.tsx and the pipeline
    // drawer waive. Non-vacuous: the same fixture fails unwaived.
    const splitter = (
      <div
        role="separator"
        tabIndex={0}
        aria-orientation="vertical"
        aria-valuenow={300}
        aria-valuemin={220}
        aria-valuemax={560}
        aria-label="Resize panel"
      />
    );
    const unwaived = render(<div>{splitter}</div>);
    await expect(
      expectNoViolationsStrict(unwaived.container, "splitter"),
    ).rejects.toThrow(/focus-order-semantics/);
    cleanup();
    const waived = render(<div>{splitter}</div>);
    await expect(
      expectNoViolationsStrict(waived.container, "splitter", [
        {
          rule: "focus-order-semantics",
          node: "role=separator",
          reason: "the window-splitter pattern, pinned by its keyboard tests",
        },
      ]),
    ).resolves.toBeUndefined();
  });

  it("covers the node it names and no other", async () => {
    // The hostile shape: the waived splitter AND a roleless focusable the
    // surface really did get wrong. A rule waived for the whole surface
    // would hide the second one — which is how a net stops judging what it
    // says it judges.
    const { container } = render(
      <div>
        <div
          role="separator"
          tabIndex={0}
          aria-orientation="vertical"
          aria-valuenow={300}
          aria-valuemin={220}
          aria-valuemax={560}
          aria-label="Resize panel"
        />
        <div tabIndex={0}>focusable, no widget role</div>
      </div>,
    );
    await expect(
      expectNoViolationsStrict(container, "splitter + a real one", [
        {
          rule: "focus-order-semantics",
          node: "role=separator",
          reason: "the window-splitter pattern, pinned by its keyboard tests",
        },
      ]),
    ).rejects.toThrow(/focus-order-semantics/);
  });

  it("fails when it matched nothing — a dead waiver is how a net stops judging", async () => {
    const { container } = render(<button type="button">ok</button>);
    await expect(
      expectNoViolationsStrict(container, "clean", [
        {
          rule: "heading-order",
          node: "role=heading",
          reason: "nothing here reports it",
        },
      ]),
    ).rejects.toThrow(/matched nothing/);
  });

  it("auto-waives label-content-name-mismatch, because axe cannot decide it", async () => {
    // The qualified-name pattern this slice landed everywhere: visible
    // "Remove", accessible name "Remove server tools_db". Non-vacuous — the
    // rule DOES report on it, identically to a real mismatch, which is why
    // no entry can act on it.
    const { container } = render(
      <button type="button" aria-label="Remove server tools_db">
        Remove
      </button>,
    );
    const raw = await axe.run(container, {
      runOnly: { type: "rule", values: ["label-content-name-mismatch"] },
      rules: { "label-content-name-mismatch": { enabled: true } },
    });
    expect(raw.incomplete.map((r) => r.id)).toContain(
      "label-content-name-mismatch",
    );
    await expect(
      expectNoViolationsStrict(container, "qualified name"),
    ).resolves.toBeUndefined();
  });
});

// Every axe entry in the studio goes through this one helper: that is what
// makes "incompletes are never dropped" true of the whole suite rather than
// of one file. A second, hand-rolled axe.run would reopen the hole in
// silence — so the helper is the only place allowed to call it.
describe("one net, no second one", () => {
  it("scans a non-trivial number of source modules, test files included", () => {
    // Guards against a glob-scope regression making the next assertions
    // vacuous — a lax net would live in a TEST file, so test files must be
    // in scope.
    const paths = allFiles.map(([path]) => path);
    // A floor, not a census: ~1465 modules today, and the realistic
    // regression is a glob that loses a whole extension or directory, not
    // one file. A surgical exclusion would slip past this — the assertions
    // below are what hold the named files.
    expect(paths.length).toBeGreaterThan(1200);
    expect(paths).toContain("/src/__tests__/a11y/primitives.test.tsx");
    expect(paths).toContain("/src/__tests__/a11y/axeHelpers.test.tsx");
    // Vite's glob excludes the module that declares it, so the scanner
    // itself is the one file the list cannot hold — covered below by text.
    expect(paths).not.toContain("/src/__tests__/a11y/_scanner.ts");
  });

  it("covers the two files the scan cannot reach", () => {
    // The scanner declares the glob, so it is absent from its own result;
    // and this file is allowlisted below because it legitimately runs axe.
    // Both are read as text instead, which is the point: a lax net added to
    // the net's own modules would otherwise be the one that gets through.
    expect(scannerSource).toContain("import.meta.glob");
    expect(/axe-core/.test(scannerSource)).toBe(false);
    expect(/\bexpectNoViolations/.test(scannerSource)).toBe(false);
    // Exactly the two probes this file declares — a third is a deliberate
    // act that has to be argued for here. The call form is what counts:
    // `axe.run()` also appears in prose below, and a call nobody awaits
    // nets nothing.
    expect(selfSource.match(/await axe\.run\s*\(/g) ?? []).toHaveLength(3);
    // No "no lax export" check here: a spec file cannot carry one, so the
    // assertion would read as a guard and prove nothing. The scan below
    // covers the helper itself, which is where a lax variant would return.
  });

  it("imports axe-core in the net's own files and nowhere else", () => {
    // The import is the chokepoint, not one spelling of the call: a second
    // net reached through `import { run } from "axe-core"` or
    // `import * as axeCore` would pass a grep for `axe.run(` and reopen the
    // hole #2591 closed. The net's own two files: the helper, and this guard
    // — which runs a raw rule to prove the allowlist above is not judging a
    // clean fixture.
    const NET = [
      "/src/__tests__/a11y/axeHelpers.ts",
      "/src/__tests__/a11y/axeHelpers.test.tsx",
    ];
    const hits = allFiles
      .filter(([path]) => !NET.includes(path))
      .filter(([, src]) => /from "axe-core"|require\("axe-core"\)/.test(src))
      .map(([path]) => path);
    if (hits.length) {
      throw new Error(
        `axe-core belongs to axeHelpers.ts: a second net would drop incompletes again (#2591).\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("is what the suites actually use — dozens of strict entries", () => {
    const entries = allFiles.reduce(
      (n, [, src]) => n + (src.match(/expectNoViolationsStrict\(/g)?.length ?? 0),
      0,
    );
    expect(entries).toBeGreaterThan(40);
    // No lax sibling survives: the hole was a helper that read
    // results.violations only.
    const lax = allFiles
      .filter(([, src]) => /\bexpectNoViolations\s*\(/.test(src))
      .map(([path]) => path);
    expect(lax).toHaveLength(0);
  });
});

// The seam that replaced five hand-rolled copies (#2591). axe has no rule
// for two controls answering to one name, so this helper IS the net for the
// repeated-row-action class — which makes it the guard of a guard again: a
// reader that mis-reads a name in either direction turns the pins built on
// it into decoration.
describe("expectUniqueAccessibleNames — the collision axe has no rule for", () => {
  it("fails on two controls answering to one name", () => {
    const { container } = render(
      <main>
        <table>
          <tbody>
            <tr>
              <td>ANTHROPIC_KEY</td>
              <td>
                <button type="button">Delete</button>
              </td>
            </tr>
            <tr>
              <td>OPENAI_KEY</td>
              <td>
                <button type="button">Delete</button>
              </td>
            </tr>
          </tbody>
        </table>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "two rows", { roles: ["button"], count: 2 }),
    ).toThrow(/sharing one accessible name/);
    expect(() =>
      expectUniqueAccessibleNames(container, "two rows", { roles: ["button"], count: 2 }),
    ).toThrow(/button "Delete" — 2 controls answer to it/);
  });

  it("passes once each row qualifies its own control", () => {
    const { container } = render(
      <main>
        <button type="button" aria-label="Delete ANTHROPIC_KEY">
          Delete
        </button>
        <button type="button" aria-label="Delete OPENAI_KEY">
          Delete
        </button>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "qualified", {
        roles: ["button"],
        expected: ["Delete ANTHROPIC_KEY", "Delete OPENAI_KEY"],
      }),
    ).not.toThrow();
  });

  // The four mechanisms. `aria-label ?? textContent` — what the five copies
  // read — reports a correctly-named control on three of them.
  it("reads a name given by aria-labelledby, not just by aria-label", () => {
    const { container } = render(
      <main>
        <span id="scope-label">Scope</span>
        <div role="radiogroup" aria-labelledby="scope-label">
          <input type="radio" name="scope" aria-label="Global" />
        </div>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "radiogroup", { roles: ["radiogroup"], count: 1 }),
    ).not.toThrow();
  });

  it("reads a name given by the control's own <label>", () => {
    const { container } = render(
      <main>
        <label htmlFor="host">Allowed host</label>
        <input id="host" type="text" />
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "labelled field", { roles: ["textbox"], count: 1 }),
    ).not.toThrow();
  });

  it("reads a name given by title alone — axe's `label` rule accepts one", () => {
    const { container } = render(
      <main>
        <button type="button" title="Poll now" />
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "title-named", { roles: ["button"], count: 1 }),
    ).not.toThrow();
  });

  it("normalises a name split across nodes and drops the aria-hidden glyph", () => {
    const { container } = render(
      <main>
        <button type="button">
          <span aria-hidden="true">▶</span> Launch <span>run</span>
        </button>
        <button type="button">
          <span aria-hidden="true">▶</span> Launch <span>run</span>
        </button>
      </main>,
    );
    // `textContent` un-normalised is "▶ Launch\n  run": the real algorithm
    // answers to neither that nor the glyph, so the read has to normalise
    // AND skip the hidden subtree — otherwise the sweep fails as "not found"
    // instead of as the collision it is.
    expect(() =>
      expectUniqueAccessibleNames(container, "split name", { roles: ["button"], count: 2 }),
    ).toThrow(/button "Launch run" — 2 controls answer to it/);
  });

  it("reads an icon-only control named by the glyph's own aria-label", () => {
    const { container } = render(
      <main>
        <button type="button">
          <span role="img" aria-label="Rotate ANTHROPIC_KEY" />
        </button>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "glyph-named", { roles: ["button"], count: 1 }),
    ).not.toThrow();
  });

  it("refuses a placeholder-only control — a placeholder is not a name (#2591)", () => {
    const { container } = render(
      <main>
        <input type="text" placeholder="Search secrets…" />
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "placeholder only", {
        roles: ["textbox"],
        count: 1,
      }),
    ).toThrow(/a placeholder is not a name/);
  });

  it("throws a reading error, never a pass, when the algorithm answers to something else", () => {
    // This reader loads no stylesheet and reads no inline style; the
    // algorithm drops a `display: none` descendant. A reader that disagrees
    // with the algorithm must say so — the alternative is a sweep that
    // silently judges names nobody hears.
    const { container } = render(
      <main>
        <button type="button">
          <span style={{ display: "none" }}>Legacy </span>Save
        </button>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "mis-read", { roles: ["button"], count: 1 }),
    ).toThrow(/reading error, never a pass/);
  });

  it("reads the contents of the roles the algorithm names by them, and of no other", () => {
    // A `role="combobox"` showing its value is named by nothing: the reader
    // agrees with the algorithm and reports the control UNNAMED rather than
    // offering a name nobody hears.
    const { container } = render(
      <main>
        <div role="combobox" tabIndex={0}>
          anthropic/claude
        </div>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "value-only", { roles: ["combobox"], count: 1 }),
    ).toThrow(/a combobox carries no name to read/);
  });

  it("drops an aria-hidden glyph from a referenced or associated label, as the algorithm does", () => {
    const { container } = render(
      <main>
        <span id="lbl">
          <span aria-hidden="true">★</span>Name
        </span>
        <input aria-labelledby="lbl" type="text" />
        <label htmlFor="host">
          <span aria-hidden="true">★</span>Host
        </label>
        <input id="host" type="text" />
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "glyphs", {
        roles: ["textbox"],
        expected: ["Name", "Host"],
      }),
    ).not.toThrow();
  });

  it("judges each role on its own — one name on a button and a checkbox is two announcements", () => {
    const { container } = render(
      <main>
        <button type="button">Draft</button>
        <label htmlFor="draft">Draft</label>
        <input id="draft" type="checkbox" />
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "two roles", {
        roles: ["button", "checkbox"],
        count: 2,
      }),
    ).not.toThrow();
  });

  // `matching` carries the surface's COUNT claim and nothing else. A scope
  // that also hid collisions would be blind to the one regression this net
  // exists for — a NEW repeated row action, whose name is by construction a
  // name no existing scope mentions.
  const scopedRows = (): HTMLElement =>
    render(
      <main>
        <button type="button">Open run A</button>
        <button type="button">Open run B</button>
        {/* Two legitimate repeats: every row's avatar runs the same filter. */}
        <button type="button" aria-label="Filter by bot">
          avatar
        </button>
        <button type="button" aria-label="Filter by bot">
          avatar
        </button>
      </main>,
    ).container;

  it("exempts only the names the surface declares repeatable, and keeps the count scoped", () => {
    const container = scopedRows();
    expect(() =>
      expectUniqueAccessibleNames(container, "scoped", {
        roles: [
          { role: "button", matching: [/^Open run /], repeatable: [/^Filter by /] },
        ],
        count: 2,
      }),
    ).not.toThrow();
  });

  it("judges a repeat the arithmetic scope does not mention", () => {
    const container = scopedRows();
    expect(() =>
      expectUniqueAccessibleNames(container, "scoped", {
        roles: [{ role: "button", matching: [/^Open run /] }],
        count: 2,
      }),
    ).toThrow(/button "Filter by bot" — 2 controls answer to it/);
  });

  it("refuses an unnamed control the arithmetic scope does not mention", () => {
    const { container } = render(
      <main>
        <button type="button">Open run A</button>
        <button type="button" />
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "scoped", {
        roles: [{ role: "button", matching: [/^Open run /] }],
        count: 1,
      }),
    ).toThrow(/carries no name to read/);
  });

  // `roles` carries the surface's ARITHMETIC claim; it is not the guard's
  // eyes. The regression this net exists for is a control a later change
  // ADDS, and its role is by construction one nobody declared — every pin on
  // the five row surfaces #2591 qualified declares `button` alone.
  it("judges a collision in a role the surface never declared", () => {
    const { container } = render(
      <main>
        <button type="button" aria-label="Rotate ANTHROPIC_KEY">
          Rotate
        </button>
        <button type="button" aria-label="Rotate OPENAI_KEY">
          Rotate
        </button>
        <input type="checkbox" aria-label="Select row" />
        <input type="checkbox" aria-label="Select row" />
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "rows", { roles: ["button"], count: 2 }),
    ).toThrow(/checkbox "Select row" — 2 controls answer to it/);
  });

  it("keeps the undeclared roles out of the arithmetic claim", () => {
    const { container } = render(
      <main>
        <button type="button">Rotate A</button>
        <button type="button">Rotate B</button>
        <input type="checkbox" aria-label="Select A" />
        <input type="text" aria-label="Rename A" />
      </main>,
    );
    // The count is the two buttons: a number that moved with every widget on
    // the surface would be one its author could not state.
    expect(() =>
      expectUniqueAccessibleNames(container, "rows", { roles: ["button"], count: 2 }),
    ).not.toThrow();
  });

  it("leaves an unnamed control in an undeclared role to axe and the source scans", () => {
    const { container } = render(
      <main>
        <button type="button">Rotate A</button>
        <input type="checkbox" />
      </main>,
    );
    // axe's `label` judges it in the entry beside this one; a pin reddening
    // here would make it its whole surface's auditor.
    expect(() =>
      expectUniqueAccessibleNames(container, "rows", { roles: ["button"], count: 1 }),
    ).not.toThrow();
  });

  it("leaves an unlabelled <select> in an undeclared role to axe — its options are not a name", () => {
    const { container } = render(
      <main>
        <button type="button">Rotate A</button>
        <select>
          <option>Alpha</option>
          <option>Beta</option>
        </select>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "rows", { roles: ["button"], count: 1 }),
    ).not.toThrow();
    // Declared, the same control is the author's to see.
    expect(() =>
      expectUniqueAccessibleNames(container, "rows", { roles: ["button", "combobox"], count: 2 }),
    ).toThrow(/a combobox carries no name to read/);
  });

  it("leaves a control it mis-reads in an undeclared role to axe, and throws on a declared one", () => {
    const { container } = render(
      <main>
        <button type="button">Rotate A</button>
        <a href="/x">
          <span style={{ display: "none" }}>Legacy </span>Open
        </a>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "rows", { roles: ["button"], count: 1 }),
    ).not.toThrow();
    expect(() =>
      expectUniqueAccessibleNames(container, "rows", { roles: ["button", "link"], count: 2 }),
    ).toThrow(/reading error, never a pass/);
  });

  // An option is reached through its listbox, a radio through its group, a
  // tab through its tablist, a menu item through its menu: the items of two
  // composites are distinct announcements, and a surface with two labelled
  // <select>s sharing an option text owes no waiver.
  it("judges an option, radio, tab or menu item among the items of its own composite", () => {
    const { container } = render(
      <main>
        <button type="button">Save</button>
        <label htmlFor="s1">Scope A</label>
        <select id="s1">
          <option>global</option>
          <option>project</option>
        </select>
        <label htmlFor="s2">Scope B</label>
        <select id="s2">
          <option>global</option>
          <option>project</option>
        </select>
        <fieldset>
          <legend>Notify</legend>
          <label>
            <input type="radio" name="notify" /> Yes
          </label>
          <label>
            <input type="radio" name="notify" /> No
          </label>
        </fieldset>
        <fieldset>
          <legend>Archive</legend>
          <label>
            <input type="radio" name="archive" /> Yes
          </label>
          <label>
            <input type="radio" name="archive" /> No
          </label>
        </fieldset>
        <div role="tablist" aria-label="Run">
          <button type="button" role="tab">
            Overview
          </button>
        </div>
        <div role="tablist" aria-label="Bot">
          <button type="button" role="tab">
            Overview
          </button>
        </div>
        <div role="menu" aria-label="Run">
          <div role="menuitem" tabIndex={-1}>
            Open
          </div>
        </div>
        <div role="menu" aria-label="Bot">
          <div role="menuitem" tabIndex={-1}>
            Open
          </div>
        </div>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "composites", { roles: ["button"], count: 1 }),
    ).not.toThrow();
    // Declared, an item is scoped the same way: the count is the items read.
    expect(() =>
      expectUniqueAccessibleNames(container, "composites", { roles: ["tab"], count: 2 }),
    ).not.toThrow();
  });

  it("still fails two items of ONE composite answering to one name", () => {
    const { container } = render(
      <main>
        <button type="button">Save</button>
        <label htmlFor="s1">Scope</label>
        <select id="s1">
          <option>global</option>
          <option>global</option>
        </select>
        <div role="radiogroup" aria-label="Notify">
          <input type="radio" aria-label="Yes" />
          <input type="radio" aria-label="Yes" />
        </div>
        <div role="tablist" aria-label="Run">
          <button type="button" role="tab">
            Overview
          </button>
          <button type="button" role="tab">
            Overview
          </button>
        </div>
      </main>,
    );
    const sweep = () =>
      expectUniqueAccessibleNames(container, "one composite", { roles: ["button"], count: 1 });
    expect(sweep).toThrow(/option "global" — 2 controls answer to it in one <select>/);
    expect(sweep).toThrow(/radio "Yes" — 2 controls answer to it in one <div role="radiogroup">/);
    expect(sweep).toThrow(/tab "Overview" — 2 controls answer to it in one <div role="tablist">/);
  });

  it("groups native radios by name when no radiogroup scopes them", () => {
    const { container } = render(
      <main>
        <button type="button">Save</button>
        <input type="radio" name="notify" aria-label="Yes" />
        <input type="radio" name="notify" aria-label="Yes" />
        <input type="radio" name="archive" aria-label="Yes" />
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "native groups", { roles: ["button"], count: 1 }),
    ).toThrow(/radio "Yes" — 2 controls answer to it$/m);
  });

  it("reports a repeatable pattern that exempted nothing — a waiver nobody can check", () => {
    const { container } = render(
      <main>
        <button type="button">Open run A</button>
        <button type="button">Open run B</button>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "stale waiver", {
        roles: [{ role: "button", repeatable: [/^Filter by /] }],
        count: 2,
      }),
    ).toThrow(/repeatable patterns that exempted nothing/);
  });

  it("refuses a sweep that declares nothing — an empty surface would pass it", () => {
    const { container } = render(<main />);
    expect(() =>
      expectUniqueAccessibleNames(container, "vacuous", { roles: ["button"] }),
    ).toThrow(/declares a role and what it read/);
    // And the declaration bites: nothing rendered, so the floor fails.
    expect(() =>
      expectUniqueAccessibleNames(container, "vacuous", { roles: ["button"], atLeast: 1 }),
    ).toThrow(/read 0 control\(s\), fewer than the 1 declared/);
  });

  // Each of these is the vacuous sweep spelled as a declaration: it reads
  // nothing, finds no collision, and passes. The guard has to refuse the
  // SHAPE, not just the absent key.
  it("refuses a claim of zero, and a sweep with no role at all", () => {
    const { container } = render(<main />);
    for (const scope of [
      { roles: ["button"], expected: [] },
      { roles: ["button"], count: 0 },
      { roles: ["button"], atLeast: 0 },
      { roles: [], count: 2 },
    ]) {
      expect(() => expectUniqueAccessibleNames(container, "vacuous", scope)).toThrow(
        /declares a role and what it read/,
      );
    }
  });

  it("fails when the names read are not the ones the surface declares", () => {
    const { container } = render(
      <main>
        <button type="button">Rotate ANTHROPIC_KEY</button>
      </main>,
    );
    expect(() =>
      expectUniqueAccessibleNames(container, "drifted", {
        roles: ["button"],
        expected: ["Rotate ANTHROPIC_KEY", "Delete ANTHROPIC_KEY"],
      }),
    ).toThrow(/the names read are not the ones declared/);
  });
});
