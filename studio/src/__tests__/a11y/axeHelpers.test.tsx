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
          marker: 'aria-labelledby="does-not-exist"',
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
          marker: 'role="separator"',
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
          marker: 'role="separator"',
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
          marker: "<h2",
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
    expect(/\bexport (async )?function expectNoViolations\s*\(/.test(selfSource)).toBe(
      false,
    );
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
