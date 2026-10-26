// Shared a11y test helpers for the jsdom + axe-core suites (primitives.test +
// views.test). NOT a test file — imported by them.
import axe from "axe-core";
import { expect } from "vitest";

/**
 * jsdom does not implement matchMedia; components that read the viewport
 * (DesktopOnlyNotice) or the theme store need a stub so they don't crash on
 * mount. Call once at module top in a jsdom test file.
 */
export function setupMatchMedia(): void {
  if (typeof window !== "undefined" && !window.matchMedia) {
    Object.defineProperty(window, "matchMedia", {
      writable: true,
      value: (query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
        dispatchEvent: () => false,
      }),
    });
  }
}

/**
 * Run axe-core over `container` against the WCAG 2.1 A + AA rule sets and
 * throw a readable per-violation summary on failure (`label` names the
 * surface under test).
 */
export async function expectNoViolations(
  container: HTMLElement,
  label: string,
): Promise<void> {
  const results = await axe.run(container, {
    runOnly: {
      type: "tag",
      values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"],
    },
  });
  if (results.violations.length > 0) {
    const summary = results.violations
      .map(
        (v) =>
          `  - [${v.id}] ${v.help} (${v.nodes.length} node${v.nodes.length > 1 ? "s" : ""})`,
      )
      .join("\n");
    throw new Error(`${label} has axe violations:\n${summary}`);
  }
  expect(results.violations).toHaveLength(0);
}

/**
 * axe "incomplete" means needs-review — dropping it is the hole this net
 * exists to close (axe 4.13 passes placeholder-only inputs outright, so the
 * label rule's silent incompletes hide unnamed controls). A surface waives a
 * rule per entry, naming the colocated test that pins the concern instead;
 * an unwaived incomplete fails the same way a violation does.
 */
export async function expectNoViolationsStrict(
  container: HTMLElement,
  label: string,
  waivers: { rule: string; reason: string }[] = [],
): Promise<void> {
  const results = await axe.run(container, {
    runOnly: {
      type: "tag",
      values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"],
    },
  });
  const violations = results.violations
    .map(
      (v) =>
        `  - [${v.id}] ${v.help} (${v.nodes.length} node${v.nodes.length > 1 ? "s" : ""})`,
    )
    .join("\n");
  if (results.violations.length > 0) {
    throw new Error(`${label} has axe violations:\n${violations}`);
  }
  // jsdom cannot compute colour contrast (no canvas), so axe marks every
  // element's color-contrast incomplete — an environment artefact, covered
  // at the token level by contrast.test.ts. Auto-waived here, visibly.
  const ENVIRONMENT_INHERENT = new Set(["color-contrast"]);
  const waived = new Set([...waivers.map((w) => w.rule), ...ENVIRONMENT_INHERENT]);
  const open = results.incomplete.filter((v) => !waived.has(v.id));
  if (open.length > 0) {
    const summary = open
      .map((v) => {
        const waiver = waivers.find((w) => w.rule === v.id);
        const hint = waiver ? ` (waived: ${waiver.reason})` : "";
        return `  - [${v.id}] ${v.help} — needs review${hint} (${v.nodes.length} node${v.nodes.length > 1 ? "s" : ""})`;
      })
      .join("\n");
    const waivedList = waivers.length
      ? `\n  waived here: ${waivers.map((w) => w.rule).join(", ")}`
      : "";
    throw new Error(
      `${label} has axe results needing review — decide each, never drop them:${waivedList}\n${summary}`,
    );
  }
  expect(open).toHaveLength(0);
}
