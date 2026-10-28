// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import axe from "axe-core";
import { afterEach, describe, expect, it } from "vitest";

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
        { rule: "aria-valid-attr-value", reason: "pinned by AdminNav.test.tsx" },
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
  it("covers WCAG 2.2 AA — target-size is in scope, even though jsdom cannot judge it", () => {
    expect([...AXE_WCAG_TAGS]).toContain("wcag22aa");
    const inScope = axe.getRules([...AXE_WCAG_TAGS]).map((r) => r.ruleId);
    expect(inScope).toContain("target-size");
    // Honest about the limit the tag buys us: with no layout, jsdom measures
    // every element 0×0 and axe reads that as a pass rather than as "cannot
    // tell" — the scope is declared here, the judgement needs #2765's engine.
    const { container } = render(<button type="button">ok</button>);
    expect(container).toBeTruthy();
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
    const off = Object.keys(
      (axeRunOptions().rules ?? {}) as Record<string, unknown>,
    );
    const bestPractice = axe.getRules(["best-practice"]).map((r) => r.ruleId);
    for (const rule of bestPractice) {
      if (AXE_BEST_PRACTICE_RULES.includes(rule as never)) {
        expect(off).not.toContain(rule);
      } else {
        expect(off).toContain(rule);
      }
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
});

// Every axe entry in the studio goes through this one helper: that is what
// makes "incompletes are never dropped" true of the whole suite rather than
// of one file. A second, hand-rolled axe.run would reopen the hole in
// silence — so the helper is the only place allowed to call it.
describe("one net, no second one", () => {
  const RAW = import.meta.glob("/src/**/*.{ts,tsx}", {
    query: "?raw",
    import: "default",
    eager: true,
  }) as Record<string, string>;

  it("scans a non-trivial number of source modules, test files included", () => {
    // Guards against a glob-scope regression making the next assertion
    // vacuous — a lax net would live in a TEST file, so test files must be
    // in scope.
    expect(Object.keys(RAW).length).toBeGreaterThan(300);
    expect(Object.keys(RAW)).toContain("/src/__tests__/a11y/primitives.test.tsx");
    // Vite's glob excludes the importing module, so this file is the one
    // place the guard below structurally cannot see: it is allowlisted there
    // too, so the guard keeps working if that ever changes.
    expect(Object.keys(RAW)).not.toContain(
      "/src/__tests__/a11y/axeHelpers.test.tsx",
    );
  });

  it("calls axe.run() from axeHelpers.ts and nowhere else", () => {
    // The net's own two files: the helper, and this guard — which runs a
    // raw rule to prove the allowlist above is not judging a clean fixture.
    const NET = [
      "/src/__tests__/a11y/axeHelpers.ts",
      "/src/__tests__/a11y/axeHelpers.test.tsx",
    ];
    const hits = Object.entries(RAW)
      .filter(([path]) => !NET.includes(path))
      .filter(([, src]) => /\baxe\.run\s*\(/.test(src))
      .map(([path]) => path);
    if (hits.length) {
      throw new Error(
        `axe.run() belongs to axeHelpers.ts: a second net would drop incompletes again (#2591).\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("is what the suites actually use — dozens of strict entries", () => {
    const entries = Object.values(RAW).reduce(
      (n, src) => n + (src.match(/expectNoViolationsStrict\(/g)?.length ?? 0),
      0,
    );
    expect(entries).toBeGreaterThan(40);
    // No lax sibling survives: the hole was a helper that read
    // results.violations only.
    const lax = Object.entries(RAW)
      .filter(([, src]) => /\bexpectNoViolations\s*\(/.test(src))
      .map(([path]) => path);
    expect(lax).toHaveLength(0);
  });
});
