// Shared a11y test helpers for the jsdom + axe-core suites (primitives.test +
// views.test). NOT a test file — imported by them.
import { within } from "@testing-library/dom";
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
  // Half of #2591's class: a control named only by `title` (or by
  // `aria-describedby`). In axe 4.13 the rule is `none: ["title-only"]` over
  // `!labelText && !!(title || ariaDescribedBy)` — it never reads
  // `placeholder`, and `label`'s own `any` list accepts
  // `non-empty-placeholder`, so a placeholder-only control passes BOTH rules
  // outright. The rendered net therefore does not see what the source scan
  // sees, and _interactive.ts is load-bearing rather than a belt on a brace.
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
 * Only for a STANDALONE block: every clamp nested in a control carries
 * focusable={false} instead (the control is the one tab stop and its name
 * carries the text, #2803), is no focus stop at all, and must not be
 * waived — the source guard in jsx-content.test.ts holds each nested call
 * site to that opt-out, and ClampText.test.tsx audits the composition with
 * no waiver.
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

/**
 * Two controls announcing the SAME accessible name — the repeated row action
 * ("Delete", "Rotate", "Replay" on every row) that is #2591's class. axe has
 * no rule for it at any level, so a colocated pin is the only thing that
 * bites, and five suites hand-rolled the same sweep before this existed
 * (Toolbar, Runs/detail, Runs/conversation, Runs/runList, ChatDock). They
 * were not interchangeable copies: three enumerated their names, one read
 * the candidate off the markup because the surface is too wide to enumerate,
 * and one scoped itself to the controls that act on ONE row. The seam has to
 * express all three or the copies stay.
 *
 * The discipline the copies earned, kept here: the name is READ off the
 * element and then handed straight back to the real accessible-name
 * algorithm (`getAllByRole`'s `name` option). A name this helper computed
 * itself would prove nothing, and a candidate the algorithm does not answer
 * to is a READING ERROR that throws — never a quiet pass.
 */
export interface UniqueNamesRole {
  role: string;
  /**
   * The controls the sweep's ARITHMETIC claim (`expected`, `count`,
   * `atLeast`) is about, for a surface whose other controls are not the
   * author's subject: the row actions among a toolbar's buttons, say.
   *
   * It narrows what the count CLAIMS, never what the guard SEES — the
   * collision verdict below judges every control the role sweep returned. A
   * scope that also hid collisions would make this net blind to the one
   * regression it exists for: a NEW repeated row action, whose name is by
   * construction a name no existing scope mentions.
   */
  matching?: readonly RegExp[];
  /**
   * The names allowed to repeat, declared at the call site because the
   * surface is the only thing that knows which of its controls act on one
   * row: N identical controls all doing the same single thing (every row's
   * bot-filter avatar) is not a defect. An entry that exempts nothing is
   * reported — a waiver nobody can check is how a net like this rots.
   */
  repeatable?: readonly RegExp[];
}

/**
 * The widget roles the COLLISION verdict sweeps whatever a call site
 * declares. Every role here is one a user operates and whose name is
 * therefore an announcement the surface owes to be distinct; a composite
 * (`table`, `tablist`, `listbox`) is out, its items are in — judged among
 * the items of their OWN composite ({@link COMPOSITE_OF}).
 *
 * `roles` stays required at the call site: it is the population the
 * arithmetic claim is about, and a sweep with nothing declared could not
 * state a count. This list is what stops that declaration from also deciding
 * what the guard SEES.
 */
const SWEPT_WIDGET_ROLES = [
  "button",
  "link",
  "checkbox",
  "radio",
  "switch",
  "textbox",
  "searchbox",
  "combobox",
  "spinbutton",
  "slider",
  "option",
  "menuitem",
  "menuitemcheckbox",
  "menuitemradio",
  "tab",
] as const;

/**
 * The roles whose CONTENTS are part of their accessible name — the
 * name-from-content step of the algorithm. A `textbox`, `combobox`,
 * `spinbutton` or `slider` is named by nothing it contains: a `<select>`
 * showing its options or a `role="combobox"` showing its value is as unnamed
 * as an empty one, and a reader that offered their text would report a name
 * nobody hears.
 */
const NAMED_BY_CONTENT = new Set([
  "button",
  "link",
  "checkbox",
  "radio",
  "switch",
  "option",
  "tab",
  "menuitem",
  "menuitemcheckbox",
  "menuitemradio",
]);

/**
 * The composite an item's name is scoped by. A user reaches an option
 * through its listbox, a radio through its group, a tab through its tablist
 * and a menu item through its menu, so two "Overview" tabs in two tablists,
 * two "global" options in two `<select>`s or two "Yes" radios in two groups
 * are distinct announcements: the collision verdict judges such an item
 * among the items of ITS composite, and an item outside any — a defect of
 * its own, axe's — surface-wide. Native radios with no `radiogroup` form a
 * group by `name` under one form owner.
 */
const COMPOSITE_OF: Record<string, string> = {
  option: "select, datalist, [role=listbox]",
  radio: "[role=radiogroup]",
  tab: "[role=tablist]",
  menuitem: "[role=menu], [role=menubar]",
  menuitemcheckbox: "[role=menu], [role=menubar]",
  menuitemradio: "[role=menu], [role=menubar]",
};

/** The scope a collision is judged in: a composite, a native radio group's key, or `null` for the whole surface. */
function scopeOf(role: string, el: Element): Element | string | null {
  const selector = COMPOSITE_OF[role];
  if (!selector) return null;
  const composite = el.closest(selector);
  if (composite) return composite;
  if (role === "radio" && el instanceof HTMLInputElement && el.name) {
    const owner = el.form ? Array.from(el.ownerDocument.forms).indexOf(el.form) : -1;
    return `radio\u0000${owner}\u0000${el.name}`;
  }
  return null;
}

export interface UniqueNamesScope {
  /**
   * The roles whose controls the sweep COUNTS, each judged on its own: a
   * `button` and a `checkbox` carrying one name are two different
   * announcements, and `getAllByRole(role, { name })` — the algorithm this
   * helper defers to — is per-role by construction.
   *
   * It is the arithmetic claim's population, not the guard's eyes:
   * {@link SWEPT_WIDGET_ROLES} is swept for collisions either way. An item
   * of a composite (`option`, `radio`, `tab`, `menuitem*`) collides only
   * with the items of its own composite ({@link COMPOSITE_OF}): a surface
   * with two labelled `<select>`s sharing an option text needs no waiver.
   */
  roles: readonly (string | UniqueNamesRole)[];
  /** Exactly these names, each read once: the enumerated form. */
  expected?: readonly string[];
  /** The exact number of controls the sweep must have read. */
  count?: number;
  /** A floor, for a surface too wide to enumerate. */
  atLeast?: number;
}

/**
 * The name this helper READS off an element, in the accessible-name
 * algorithm's own precedence: `aria-labelledby`, then `aria-label`, then the
 * control's `<label>`, then — for a role named by its content — its
 * contents, then `title`.
 *
 * `aria-label ?? textContent` is what the copies used, and it is wrong in
 * both directions on a generic sweep: a control named by `aria-labelledby`
 * (RadioGroup's radiogroup, NavLinks, AdminNav) or by `title` alone — which
 * axe's own `label` rule accepts as `non-empty-title` — reads back empty, so
 * a correctly-named control would be reported as unnamed.
 *
 * Two deliberate departures from the algorithm, both of which surface as a
 * loud failure rather than as a pass:
 *  - `placeholder` is NOT read. The algorithm accepts it for an `<input>`
 *    and that acceptance IS #2591 (axe's `label` rule passes a
 *    placeholder-only control outright), so a control the placeholder alone
 *    names reads back empty here and is reported.
 *  - no stylesheet and no inline style is read, while the algorithm drops a
 *    `display: none` descendant: a name this reader composes from text the
 *    algorithm never sees comes back as a reading error.
 */
function readableName(el: Element, role: string): string {
  const flat = (s: string | null | undefined): string => (s ?? "").replace(/\s+/g, " ").trim();
  const labelledBy = el.getAttribute("aria-labelledby");
  if (labelledBy) {
    // accname recurses into each referenced element: its own aria-label
    // wins over its contents there exactly as it does here, and an
    // aria-hidden glyph inside it contributes nothing.
    const parts = labelledBy
      .split(/\s+/)
      .filter(Boolean)
      .map((id) => {
        const target = el.ownerDocument.getElementById(id);
        return target ? flat(target.getAttribute("aria-label") || contentsOf(target)) : "";
      })
      .filter(Boolean);
    if (parts.length > 0) return flat(parts.join(" "));
  }
  const ariaLabel = flat(el.getAttribute("aria-label"));
  if (ariaLabel) return ariaLabel;
  // Native association, the mechanism a form primitive's CONSUMER supplies
  // (`<label htmlFor>` / an enclosing label), read as the algorithm reads
  // a label's contents. `labels` is undefined on anything that is not a
  // labelable control, which is the right answer for a button.
  const labels = (el as HTMLInputElement).labels;
  if (labels && labels.length > 0) {
    const text = flat(Array.from(labels, (l) => contentsOf(l)).join(" "));
    if (text) return text;
  }
  if (NAMED_BY_CONTENT.has(role)) {
    const contents = flat(contentsOf(el));
    if (contents) return contents;
  }
  return flat(el.getAttribute("title"));
}

/**
 * The element's contents as the name-from-content traversal reads them:
 * `aria-hidden` and `hidden` subtrees contribute nothing (an icon glyph in a
 * span is not part of the button's name), a descendant carrying its own
 * `aria-label` contributes that name instead of its text, and an `<img>`
 * contributes its `alt`. Plain `textContent` disagrees with the algorithm on
 * every icon-plus-text control the studio renders, and this reader's
 * disagreements all come back as reading errors.
 */
function contentsOf(el: Element): string {
  const parts: string[] = [];
  const walk = (n: Node): void => {
    if (n.nodeType === n.TEXT_NODE) {
      parts.push(n.nodeValue ?? "");
      return;
    }
    if (n.nodeType !== n.ELEMENT_NODE) return;
    const e = n as Element;
    if (e.getAttribute("aria-hidden") === "true" || e.hasAttribute("hidden")) return;
    const own = (e.getAttribute("aria-label") ?? "").trim();
    if (own) {
      parts.push(` ${own} `);
      return;
    }
    if (e.tagName === "IMG") {
      parts.push(` ${(e.getAttribute("alt") ?? "").trim()} `);
      return;
    }
    for (const child of Array.from(e.childNodes)) walk(child);
  };
  for (const child of Array.from(el.childNodes)) walk(child);
  return parts.join("");
}

/** The element's own opening tag, for a failure a reader can act on. */
function markerOf(el: Element): string {
  return openingTag(el.outerHTML).replace(/\s+/g, " ").slice(0, 160);
}

/**
 * Fail when two of `container`'s controls answer to one accessible name.
 *
 * `scope` must carry a non-vacuity claim that can FAIL on an empty surface:
 * at least one role, and an `expected`, `count` or `atLeast` that asks for
 * something. A sweep over a surface that rendered nothing finds no collision
 * and says so, which is the one way this guard can be green while judging an
 * empty tree — and a claim of zero (`expected: []`, `count: 0`, `atLeast: 0`)
 * is that same sweep spelled as a declaration. An ABSENCE is asserted with
 * `queryByRole`, never with this helper.
 */
export function expectUniqueAccessibleNames(
  container: HTMLElement,
  label: string,
  scope: UniqueNamesScope,
): void {
  const claims = [
    scope.expected === undefined ? undefined : scope.expected.length,
    scope.count,
    scope.atLeast,
  ].filter((n): n is number => n !== undefined);
  if (scope.roles.length === 0 || claims.length === 0 || claims.every((n) => n === 0)) {
    throw new Error(
      `${label}: a unique-names sweep declares a role and what it read (a non-empty expected, count or atLeast) — a claim of zero cannot fail on a surface that rendered nothing; assert an absence with queryByRole`,
    );
  }
  const q = within(container);
  const read: string[] = [];
  const collisions = new Map<string, string>();
  /** The `repeatable` entries that actually exempted a repeat, per role. */
  const spent = new Set<string>();
  const declaredRepeatable: { role: string; re: RegExp }[] = [];
  const declaredRoles = new Set(
    scope.roles.map((entry) => (typeof entry === "string" ? entry : entry.role)),
  );
  // The declared roles, then every OTHER widget role swept for collisions
  // alone: a pin names the roles its author had in mind, and the control a
  // later change adds is by construction one nobody declared — the five row
  // surfaces #2591 qualified all declare `button`, so a repeated `checkbox`
  // or `textbox` beside their buttons would answer to no sweep at all. This
  // is the blindness `matching` already refuses, one level up.
  const sweeps: { entry: string | UniqueNamesRole; counts: boolean }[] = [
    ...scope.roles.map((entry) => ({ entry, counts: true })),
    ...SWEPT_WIDGET_ROLES.filter((role) => !declaredRoles.has(role)).map((role) => ({
      entry: role as string | UniqueNamesRole,
      counts: false,
    })),
  ];
  for (const { entry, counts } of sweeps) {
    const role = typeof entry === "string" ? entry : entry.role;
    const matching = typeof entry === "string" ? undefined : entry.matching;
    const repeatable = typeof entry === "string" ? undefined : entry.repeatable;
    for (const re of repeatable ?? []) declaredRepeatable.push({ role, re });
    for (const el of q.queryAllByRole(role)) {
      const name = readableName(el, role);
      // Thrown BEFORE the arithmetic scope is applied: a control with no name
      // under ANY mechanism is a defect of its own (axe `button-name`,
      // `label`), and a scope that could hide one is the silent skip this net
      // refuses.
      //
      // For a DECLARED role only: an unnamed control in a role the pin never
      // asked about is that same defect, judged by axe's `label` /
      // `button-name` in the entry beside this one and by the source scans.
      // Reporting it here would make every pin its whole surface's auditor,
      // and the collision verdict below is what the undeclared sweep is for.
      if (!name) {
        if (!counts) continue;
        throw new Error(
          `${label}: a ${role} carries no name to read — aria-labelledby, aria-label, its <label>, its contents (for a role named by them) and title are all empty, and a placeholder is not a name (#2591):\n  ${markerOf(el)}`,
        );
      }
      const answering = q.queryAllByRole(role, { name });
      if (!answering.includes(el)) {
        // A DECLARED role's reading error is the author's to see. In an
        // undeclared role it is a control this reader cannot judge, left to
        // axe and the source scans exactly like an unnamed one.
        if (!counts) continue;
        throw new Error(
          `${label}: the name read off a ${role} is not what the accessible-name algorithm computes — a reading error, never a pass:\n  read: ${JSON.stringify(name)}\n  ${markerOf(el)}`,
        );
      }
      // Judged on EVERY control the role sweep returned, outside `matching`:
      // the regression this net exists for is a new repeated row action, and
      // its name is by construction one no existing scope mentions. An item
      // of a composite is judged among the items of ITS composite.
      const scope = scopeOf(role, el);
      const peers =
        scope === null ? answering : answering.filter((other) => scopeOf(role, other) === scope);
      if (peers.length > 1) {
        const exempt = (repeatable ?? []).filter((re) => re.test(name));
        // Keyed by role and pattern SOURCE: one shared RegExp declared for
        // two roles is live for the role whose name repeats, and dead for
        // the other.
        for (const re of exempt) spent.add(`${role}\u0000${re.source}`);
        if (exempt.length === 0) {
          const where =
            scope instanceof Element
              ? ` in one <${scope.tagName.toLowerCase()}${scope.getAttribute("role") ? ` role="${scope.getAttribute("role")}"` : ""}>`
              : "";
          // One entry per composite: two listboxes each repeating a name
          // are two defects, reported apart.
          const first = answering.findIndex((other) => scopeOf(role, other) === scope);
          collisions.set(
            `${role}\u0000${name}\u0000${first}`,
            `  ${role} "${name}" — ${peers.length} controls answer to it${where}`,
          );
        }
      }
      // The arithmetic claim is about the DECLARED roles: a count that moved
      // with every undeclared widget on the surface would be a number its
      // author could not state.
      if (!counts) continue;
      if (matching && !matching.some((re) => re.test(name))) continue;
      read.push(name);
    }
  }

  const problems: string[] = [];
  if (collisions.size > 0) {
    problems.push(
      `controls sharing one accessible name — qualify each with the row it acts on:\n${[...collisions.values()].join("\n")}`,
    );
  }
  const dead = declaredRepeatable.filter(({ role, re }) => !spent.has(`${role}\u0000${re.source}`));
  if (dead.length > 0) {
    problems.push(
      `repeatable patterns that exempted nothing — no name on this surface repeats under them, drop them:\n${dead
        .map(({ role, re }) => `  ${role} ${re}`)
        .join("\n")}`,
    );
  }
  if (scope.expected !== undefined) {
    const declared = [...scope.expected].sort();
    const found = [...read].sort();
    if (declared.join("\n") !== found.join("\n")) {
      problems.push(
        `the names read are not the ones declared:\n  declared: ${JSON.stringify(declared)}\n  read:     ${JSON.stringify(found)}`,
      );
    }
  }
  if (scope.count !== undefined && read.length !== scope.count) {
    problems.push(`read ${read.length} control(s), not the ${scope.count} declared`);
  }
  if (scope.atLeast !== undefined && read.length < scope.atLeast) {
    problems.push(`read ${read.length} control(s), fewer than the ${scope.atLeast} declared`);
  }
  if (problems.length > 0) {
    throw new Error(`${label} — accessible names: ${problems.join("\n\n")}`);
  }
}
