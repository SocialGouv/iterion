// Shared a11y test helpers for the jsdom + axe-core suites (primitives.test +
// views.test). NOT a test file — imported by them.
import axe from "axe-core";

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
 * `wcag22aa` carries exactly one rule today, `target-size` (SC 2.5.8), which
 * axe ships OFF by default — `declaredRuleSwitches` turns it on, or the tag
 * would be a declaration nobody runs. jsdom still has no layout, so it
 * answers `inapplicable` on element after element: the scope is declared and
 * executed here, the judgement needs a real engine — it lives in
 * e2e/a11y/target-size.spec.ts (#2765).
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
 * pins, and `AXE_BEST_PRACTICE_RULES` is the one place to argue with.
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
  // A real <img> whose alt repeats the text next to it, announced twice
  // (BrandMark, avatars). The rule's selector is `img`, so the studio's
  // role="img" glyphs are out of its reach — this is the net for the brand
  // and avatar surfaces, not for them.
  "image-redundant-alt",
  // #2591's own class: a control named only by title (or placeholder, which
  // axe's `label` rule passes outright). This is the rule that makes the
  // rendered net see what the source scan sees.
  "label-title-only",
  // Two landmarks with the same role and no distinguishing name are one
  // landmark as far as navigation goes.
  "landmark-unique",
  // role="presentation"/"none" on an element that keeps its semantics
  // anyway: the author's intent and the tree disagree. It judges elements
  // with an IMPLICIT role (a <button role="presentation">), so a plain
  // <div role="presentation"> — the studio's scrims — is inapplicable: a
  // guard against the mistake rather than a finding today.
  "presentation-role-conflict",
  // RGAA 5.7: a scope= that points nowhere useful.
  "scope-attr-valid",
  // A summary that repeats the caption: the table announces itself twice.
  "table-duplicate-name",
  // RGAA 12.8: tabindex > 0 takes the focus order away from the DOM.
  "tabindex",
] as const;

/**
 * The per-rule switches that make the declaration above the scope that
 * actually RUNS.
 *
 * `runOnly` by tag is not sufficient: axe skips a rule that is
 * `experimental`, or off by default, even when its tag is asked for. Under
 * tags alone this net silently dropped `target-size` (the whole of wcag22aa,
 * `enabled: false` in axe 4.13), five WCAG A/AA rules —
 * `label-content-name-mismatch` (the only machine check for SC 2.5.3),
 * `p-as-heading`, `table-fake-caption`, `td-has-header`,
 * `css-orientation-lock` — and `focus-order-semantics` out of the declared
 * best-practice list. A rule named in a list and never executed is the same
 * hole as an incomplete nobody reads, so every declared rule is turned ON by
 * id and every undeclared best-practice rule OFF by id.
 *
 * Read from axe's registry (`getRules`) rather than from a hand-copied list,
 * so a rule an upgrade adds to a declared tag arrives in the net, and one it
 * adds to `best-practice` stays off until someone decides.
 */
export function declaredRuleSwitches(): Record<string, { enabled: boolean }> {
  const switches: Record<string, { enabled: boolean }> = {};
  const idOf = (rule: unknown) => (rule as { ruleId: string }).ruleId;
  // ON: the declared WCAG scope, then the declared best-practice rules.
  for (const rule of axe.getRules([...AXE_WCAG_TAGS])) {
    switches[idOf(rule)] = { enabled: true };
  }
  for (const id of AXE_BEST_PRACTICE_RULES) {
    switches[id] = { enabled: true };
  }
  // OFF: every other best-practice rule. A rule already ON above is in the
  // WCAG scope, which decides — turning it off here would shrink that half.
  for (const rule of axe.getRules(["best-practice"])) {
    const id = idOf(rule);
    if (switches[id]?.enabled) continue;
    switches[id] = { enabled: false };
  }
  return switches;
}

/** The axe run options every entry shares: the declared scope, nothing else. */
export function axeRunOptions(): axe.RunOptions {
  return {
    runOnly: {
      type: "tag",
      values: [...AXE_WCAG_TAGS, "best-practice"],
    },
    rules: declaredRuleSwitches(),
    // The node's own element, so a result can be identified by what it IS.
    // axe's `html` is a SERIALISATION it rewrites past 300 characters —
    // every attribute name and value truncated to 20 — and a node matched by
    // a string in that serialisation stops matching when the library next
    // adds an attribute.
    elementRef: true,
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
const VENDOR_SENTINELS: { rule: string; attribute: string; why: string }[] = [
  {
    rule: "aria-hidden-focus",
    attribute: "data-radix-focus-guard",
    why: "Radix's own focus guard: aria-hidden + tabindex=0 by design",
  },
];

/**
 * Does the node ITSELF carry `attribute`? Read from the element axe hands
 * back (`elementRef`), never from its `html`: that string is a truncating
 * serialisation, and it is the node's outerHTML — a surface's own wrapper
 * CONTAINS its library sentinels, so a substring match there drops the
 * wrapper's real violation. The element-less fallback keeps the own-tag
 * discipline for a result axe hands back without one.
 */
function nodeHasAttribute(n: axe.NodeResult, attribute: string): boolean {
  if (n.element) return n.element.hasAttribute(attribute);
  return openingTag(n.html).includes(attribute);
}

/** The node's own opening tag — `>` inside an attribute value does not end it. */
function openingTag(html: string): string {
  let quote: '"' | "'" | null = null;
  for (let i = 0; i < html.length; i += 1) {
    const c = html[i];
    if (quote) {
      if (c === quote) quote = null;
      continue;
    }
    if (c === '"' || c === "'") {
      quote = c as '"' | "'";
      continue;
    }
    if (c === ">") return html.slice(0, i + 1);
  }
  return html;
}

function withoutVendorSentinels(results: axe.Result[]): axe.Result[] {
  return results
    .map((result) => {
      const sentinels = VENDOR_SENTINELS.filter((s) => s.rule === result.id);
      if (sentinels.length === 0) return result;
      return {
        ...result,
        nodes: result.nodes.filter(
          (n) => !sentinels.some((s) => nodeHasAttribute(n, s.attribute)),
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
 * Rules no entry can decide, and what decides them instead. Auto-waived for
 * every surface, visibly, because the reason is the same everywhere:
 *
 *  - `color-contrast`: jsdom has no canvas, so axe marks every element's
 *    contrast incomplete. Judged at the token level by contrast.test.ts.
 *  - `label-content-name-mismatch`: axe 4.13 answers needs-review for EVERY
 *    control carrying both an aria-label and visible text — probed, the
 *    contained case (`Remove` → "Remove server tools_db"), the glyph case and
 *    a REAL mismatch (`Delete` → "Save") come back identical, so the rule
 *    cannot separate them. SC 2.5.3 is pinned by the colocated name tests,
 *    which assert the visible word is part of the accessible name.
 */
const UNDECIDABLE_HERE: Record<string, string> = {
  "color-contrast": "jsdom has no canvas — contrast.test.ts judges the tokens",
  "label-content-name-mismatch":
    "axe 4.13 cannot separate a contained name from a mismatch — the colocated name tests pin SC 2.5.3",
};

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
 *
 * A waiver covers BOTH buckets — a rule can be a violation on one axe release
 * and needs-review on the next, and the one lane a surface has for "this rule
 * is wrong about this pattern, and here is what judges it" must not depend on
 * that. It is scoped to the NODE it names, by an attribute that node carries,
 * for the same reason the vendor sentinels are: a rule waived for
 * a whole surface is a surface that stopped being judged, and the next
 * roleless focusable added to it would never be seen. A waiver that matched no
 * node FAILS, too: it is a live claim about this surface, and a dead one is
 * how a net quietly stops judging what it says it judges.
 */
export interface AxeWaiver {
  rule: string;
  /**
   * An attribute the waived node carries (`role`, `data-…`): read off the
   * element, so the waiver cannot drift with axe's html serialisation. With
   * `=value`, the attribute must also have that value.
   */
  node: string;
  /** Why the rule is wrong here, naming what pins the concern instead. */
  reason: string;
}

/**
 * ClampText's clamped block is a focus stop carrying no widget role on
 * purpose (RGAA 10.12: focusing it reveals what the clamp hides), so
 * `focus-order-semantics` is wrong about it where the block is the outermost
 * focusable at that node — and right about anything else the surface makes
 * focusable. Declared beside the waiver type rather than copied per entry:
 * the claim belongs to the primitive, and the copies are where its reason
 * drifts.
 *
 * NOT for a surface that nests the block inside a control: a focus stop
 * inside a button is a second tab stop for one control, which is a defect
 * and not a recovery mechanism (#2803).
 */
export const CLAMP_TEXT_WAIVER: AxeWaiver = {
  rule: "focus-order-semantics",
  node: "data-clamp-text",
  reason: "ClampText's deliberate focus stop, pinned by ClampText.test.tsx",
};

/** `role=separator` → the node carries role="separator"; `data-x` → it has the attribute. */
function nodeMatches(n: axe.NodeResult, selector: string): boolean {
  const eq = selector.indexOf("=");
  if (eq === -1) return nodeHasAttribute(n, selector);
  const name = selector.slice(0, eq);
  const value = selector.slice(eq + 1);
  if (n.element) return n.element.getAttribute(name) === value;
  return openingTag(n.html).includes(`${name}="${value}"`);
}

export async function expectNoViolationsStrict(
  container: HTMLElement,
  label: string,
  waivers: AxeWaiver[] = [],
): Promise<void> {
  const results = await axe.run(container, axeRunOptions());
  // Sentinels first, in both buckets: which bucket axe files a library's own
  // node under is axe's business and moves between releases; whose node it is
  // does not.
  const violations = withoutVendorSentinels(results.violations);
  const incomplete = withoutVendorSentinels(results.incomplete);
  const matched = new Set<AxeWaiver>();
  const keep = (result: axe.Result): axe.Result | null => {
    const mine = waivers.filter((w) => w.rule === result.id);
    if (mine.length === 0) return result;
    const nodes = result.nodes.filter((n) => {
      const hit = mine.find((w) => nodeMatches(n, w.node));
      if (hit) matched.add(hit);
      return !hit;
    });
    return nodes.length > 0 ? { ...result, nodes } : null;
  };
  const waivedList = waivers.length
    ? `\n  waived here: ${waivers.map((w) => `${w.rule} on ${w.node} (${w.reason})`).join(", ")}`
    : "";

  const problems: string[] = [];
  const failed = violations
    .map(keep)
    .filter((r): r is axe.Result => r !== null);
  if (failed.length > 0) {
    const summary = failed
      .map((v) => `  - [${v.id}] ${v.help}${nodes(v)}`)
      .join("\n");
    problems.push(`axe violations:${waivedList}\n${summary}`);
  }
  const open = incomplete
    .filter((v) => !(v.id in UNDECIDABLE_HERE))
    .map(keep)
    .filter((r): r is axe.Result => r !== null);
  if (open.length > 0) {
    const summary = open
      .map((v) => `  - [${v.id}] ${v.help} — needs review${nodes(v)}`)
      .join("\n");
    problems.push(
      `axe results needing review — decide each, never drop them:${waivedList}\n${summary}`,
    );
  }
  const dead = waivers.filter((w) => !matched.has(w));
  if (dead.length > 0) {
    problems.push(
      `waivers that matched nothing — the surface no longer needs them, drop them:\n${dead
        .map((w) => `  - [${w.rule}] on ${w.node}: ${w.reason}`)
        .join("\n")}`,
    );
  }
  if (problems.length > 0) {
    throw new Error(`${label} has ${problems.join("\n\n")}`);
  }
}
