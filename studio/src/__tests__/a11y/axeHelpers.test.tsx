// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { expectNoViolations, expectNoViolationsStrict } from "./axeHelpers";

afterEach(cleanup);

// The strict variant exists because axe "incomplete" (needs review) was
// silently dropped: the dangling-aria-labelledby hole the AdminNav entry
// conceded. Leg 1 of #2591: an incomplete fails unless the surface waives
// the rule by name.
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

  it("the legacy helper still reads violations only (existing suites keep passing)", async () => {
    const { container } = render(
      <section aria-labelledby="does-not-exist">
        <button type="button">ok</button>
      </section>,
    );
    await expect(expectNoViolations(container, "legacy")).resolves.toBeUndefined();
  });
});
