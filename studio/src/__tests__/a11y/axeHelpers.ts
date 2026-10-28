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
 * The WCAG scope every axe entry runs against — levels A + AA of 2.0, 2.1 and
 * 2.2. Declared once so widening the net is one edit, and so an axe release
 * that adds a rule to one of these tags arrives in every entry at once.
 *
 * `wcag22aa` carries exactly one rule today, `target-size` (SC 2.5.8), and
 * jsdom has no layout: every element measures 0×0, which axe reads as a
 * *pass* rather than as "cannot tell". The tag declares the scope honestly;
 * judging target size needs a real engine, which is #2765's job.
 */
export const AXE_WCAG_TAGS = [
  "wcag2a",
  "wcag2aa",
  "wcag21a",
  "wcag21aa",
  "wcag22aa",
] as const;

/**
 * The axe `best-practice` rules the suites opt into, each one because it
 * judges a mounted fragment and names a defect the WCAG tags miss. It is an
 * ALLOWLIST, not the tag: a best-practice rule nobody decided on stays off,
 * so an axe upgrade cannot redden 55 entries at once with a concern no test
 * pins, and `declaredBestPracticeRules` is the one place to argue with.
 *
 * Deliberately out:
 *  - the page-level family (`region`, `landmark-one-main`,
 *    `page-has-heading-one`, `skip-link`, `landmark-*-is-top-level`,
 *    `landmark-no-duplicate-*`): these suites mount components and single
 *    panels, never the shell, so they would report the harness rather than
 *    the surface. The shell's landmarks are outline.test.tsx's business.
 *  - `accesskeys`: page-scoped uniqueness, and the studio sets no accesskey
 *    at all (greppable: no `accessKey=` in src).
 *  - `frame-tested`, `meta-viewport-large`: no frames and no meta viewport in
 *    a mounted fragment.
 *  - `hidden-content`: incomplete by construction — it exists to say "a human
 *    should look at the hidden parts", which no gate can hold.
 */
export const AXE_BEST_PRACTICE_RULES = [
  // A role the element cannot carry makes every getByRole pin above it a
  // lie, so the net's own vocabulary depends on this one.
  "aria-allowed-role",
  // The studio's drawers and the command palette are dialogs; an unnamed
  // dialog announces nothing on open.
  "aria-dialog-name",
  // role=text swallows its focusable descendants: a button inside becomes
  // unreachable text.
  "aria-text",
  // The same unnamed-control class as #2591, on tree widgets.
  "aria-treeitem-name",
  // RGAA 9.1: an empty heading is a hole in the outline. outline.test.tsx
  // reads source; this reads what rendered.
  "empty-heading",
  // RGAA 5.7: a header cell with no text leaves its column unannounced.
  "empty-table-header",
  // A focusable element with no widget role — the unnamed-control class's
  // cousin: the screen reader announces a position, not a control.
  "focus-order-semantics",
  // RGAA 9.1: levels that skip on a composed surface.
  "heading-order",
  // An icon's text alternative repeated as its own visible label: the
  // IconButton + caption pattern's trap, announced twice.
  "image-redundant-alt",
  // #2591's own class: a control named only by title (or placeholder, which
  // axe's `label` rule passes outright). This is the rule that makes the
  // rendered net see what the source scan sees.
  "label-title-only",
  // Two landmarks with the same role and no distinguishing name are one
  // landmark as far as navigation goes.
  "landmark-unique",
  // role="presentation"/"none" on an element that keeps its semantics
  // anyway: the author's intent and the tree disagree.
  "presentation-role-conflict",
  // RGAA 5.7: a scope= that points nowhere useful.
  "scope-attr-valid",
  // A summary that repeats the caption: the table announces itself twice.
  "table-duplicate-name",
  // RGAA 12.8: tabindex > 0 takes the focus order away from the DOM.
  "tabindex",
] as const;

/**
 * Every `best-practice` rule outside the declared allowlist, as axe's own
 * per-rule switch. `runOnly` filters by TAG, so `best-practice` can only come
 * in whole and be narrowed back down from here — and the narrowing is read
 * from axe's registry (`getRules`) rather than from a hand-copied list, so a
 * rule added by an upgrade is off by default instead of missing from both.
 *
 * A best-practice rule that also carries one of the declared WCAG tags stays
 * ON: the WCAG tag already put it in scope, and disabling it here would
 * silently shrink the WCAG half.
 */
export function declaredBestPracticeRules(): Record<
  string,
  { enabled: boolean }
> {
  const wcag = new Set<string>(AXE_WCAG_TAGS);
  const declared = new Set<string>(AXE_BEST_PRACTICE_RULES);
  const off: Record<string, { enabled: boolean }> = {};
  for (const rule of axe.getRules(["best-practice"])) {
    const { ruleId, tags } = rule as unknown as {
      ruleId: string;
      tags?: string[];
    };
    if (declared.has(ruleId)) continue;
    if ((tags ?? []).some((tag) => wcag.has(tag))) continue;
    off[ruleId] = { enabled: false };
  }
  return off;
}

/** The axe run options every entry shares: the declared scope, nothing else. */
export function axeRunOptions(): axe.RunOptions {
  return {
    runOnly: {
      type: "tag",
      values: [...AXE_WCAG_TAGS, "best-practice"],
    },
    rules: declaredBestPracticeRules(),
  };
}

/**
 * Nodes a surface cannot own, because a library renders them: Radix's focus
 * guards are `aria-hidden` spans carrying `tabindex="0"` — catching Tab and
 * bouncing it back into the dialog is the pattern's whole point — and jsdom
 * has no layout, so axe cannot see they are visually hidden (opacity 0,
 * pointer-events none) and reports `aria-hidden-focus` as needs-review on
 * every open Radix dialog.
 *
 * Dropped per NODE and per rule id, never per surface: a surface's OWN
 * aria-hidden-focus mistake still fails, in the same entry, with the node
 * that caused it. A rule waiver would hide both.
 */
const VENDOR_SENTINELS: { rule: string; marker: string; why: string }[] = [
  {
    rule: "aria-hidden-focus",
    marker: "data-radix-focus-guard",
    why: "Radix's own focus guard: aria-hidden + tabindex=0 by design",
  },
];

/**
 * The node's OWN opening tag. axe reports `html` as the node's outerHTML, so
 * a marker searched in the whole string also matches a surface's own element
 * that merely CONTAINS the library's sentinel — which is how a wrapper's real
 * aria-hidden-focus violation gets dropped. The sentinel is identified by the
 * attributes it carries, so the opening tag is the whole question.
 */
function openingTag(html: string): string {
  const end = html.indexOf(">");
  return end === -1 ? html : html.slice(0, end + 1);
}

function withoutVendorSentinels(results: axe.Result[]): axe.Result[] {
  return results
    .map((result) => {
      const markers = VENDOR_SENTINELS.filter((s) => s.rule === result.id);
      if (markers.length === 0) return result;
      return {
        ...result,
        nodes: result.nodes.filter(
          (n) =>
            !markers.some((s) => openingTag(n.html).includes(s.marker)),
        ),
      };
    })
    .filter((result) => result.nodes.length > 0);
}

/**
 * The offending nodes, by the selector axe resolved them to. A rule id and a
 * count send the reader back to the browser; the selector and the first node's
 * markup are what make a failure fixable from the log alone. Capped at three
 * nodes and at one line of markup each: a surface-wide rule can match dozens.
 */
function nodes(result: axe.Result): string {
  const shown = result.nodes.slice(0, 3).map((n) => {
    const target = Array.isArray(n.target) ? n.target.join(" ") : String(n.target);
    const html = n.html.replace(/\s+/g, " ").slice(0, 120);
    return `\n      ${target}\n        ${html}`;
  });
  const more =
    result.nodes.length > shown.length
      ? `\n      … ${result.nodes.length - shown.length} more`
      : "";
  return ` (${result.nodes.length} node${result.nodes.length > 1 ? "s" : ""})${shown.join("")}${more}`;
}

/**
 * Run axe over `container` against the declared scope and throw a readable
 * summary on failure (`label` names the surface under test).
 *
 * There is no lax variant: axe "incomplete" means needs-review, and dropping
 * it is the hole this net exists to close (axe 4.13 passes placeholder-only
 * inputs outright, so the `label` rule's silent incompletes hid unnamed
 * controls). A surface waives a rule per entry, naming the colocated test
 * that pins the concern instead; an unwaived incomplete fails the same way a
 * violation does.
 */
export async function expectNoViolationsStrict(
  container: HTMLElement,
  label: string,
  waivers: { rule: string; reason: string }[] = [],
): Promise<void> {
  const results = await axe.run(container, axeRunOptions());
  // Applied to both buckets: which bucket axe files a library's own sentinel
  // under is axe's business and moves between releases; whose node it is does
  // not.
  const failed = withoutVendorSentinels(results.violations);
  if (failed.length > 0) {
    const violations = failed
      .map((v) => `  - [${v.id}] ${v.help}${nodes(v)}`)
      .join("\n");
    throw new Error(`${label} has axe violations:\n${violations}`);
  }
  // jsdom cannot compute colour contrast (no canvas), so axe marks every
  // element's color-contrast incomplete — an environment artefact, covered
  // at the token level by contrast.test.ts. Auto-waived here, visibly.
  const ENVIRONMENT_INHERENT = new Set(["color-contrast"]);
  const waived = new Set([...waivers.map((w) => w.rule), ...ENVIRONMENT_INHERENT]);
  const open = withoutVendorSentinels(results.incomplete).filter(
    (v) => !waived.has(v.id),
  );
  if (open.length > 0) {
    const summary = open
      .map((v) => `  - [${v.id}] ${v.help} — needs review${nodes(v)}`)
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
