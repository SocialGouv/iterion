// studio-a11y.target-size helper — the geometry judgement jsdom cannot make.
//
// src/__tests__/a11y/axeHelpers.ts declares wcag22aa in AXE_WCAG_TAGS, whose
// only rule today is `target-size` (SC 2.5.8). Under jsdom every element
// measures 0×0 and axe reads that as a PASS, so the rendered net is green on
// a criterion it cannot see (#2765). This module runs that ONE rule in a real
// browser and turns the result into a verdict per surface.
//
// The harness judges TWO classes of sub-24px target, because WCAG does:
//
//   - axe-violation: a target axe's rule flags — undersized AND failing the
//     spacing exception (a 24px circle centered on it meets another target).
//   - wcag-spacing-exception: a target axe PASSES, but only through the
//     spacing exception — physically under 24×24, saved by the empty room
//     around it.
//
// The policy is DECIDED (#2765, Q47b): the studio's floor is SC 2.5.8's 24px
// AA, and WCAG's exceptions are accepted where one honestly applies — so the
// second class is reported too rather than letting axe's pass swallow it,
// and a target of either class survives only behind a waiver whose
// `decision` names the exception it rests on.
//
// Either way, per node the report carries the real CSS px box (read off
// getBoundingClientRect — axe reports the rule id, not the size) and the
// WCAG exception candidates (Inline / Spacing / Equivalent). A node survives
// only if an explicit waiver covers it (surface + selector + the decided
// exception + a written justification), and a waiver that matched nothing
// fails too — a dead waiver is how a net quietly stops judging what it says
// it judges (same discipline as axeHelpers.ts).
//
// NOT a test file — imported by target-size.spec.ts.

import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";

import type { Page } from "@playwright/test";

const require = createRequire(import.meta.url);

// axe-core ships its browser bundle in the studio's own dependency tree, so
// no new package is needed. It is injected with page.evaluate of the SOURCE,
// not addScriptTag: the studio's CSP is `script-src 'self'` (pinned by
// e2e/specs/security-headers.spec.ts) and blocks the inline <script>
// addScriptTag would insert, while Playwright's evaluate runs over the CDP
// debugger channel the CSP does not govern.
const AXE_SOURCE = fs.readFileSync(
  path.join(path.dirname(require.resolve("axe-core")), "axe.min.js"),
  "utf8",
);

/** The WCAG 2.2 SC 2.5.8 AA floor, in CSS pixels. */
export const TARGET_SIZE_FLOOR_PX = 24;

/** How a sub-24px target came to the harness's attention. */
export type FindingKind = "axe-violation" | "wcag-spacing-exception";

/**
 * The WCAG 2.2 SC 2.5.8 exceptions the studio's decided policy accepts
 * (#2765, Q47b) — the three a compact control can honestly rest on:
 *
 *   - `spacing`: the target is under 24×24 but the 24px circle centered on
 *     it meets no other target (axe's `target-offset` check passes);
 *   - `inline`: the target sits inside a line of text;
 *   - `equivalent`: the same function is available through a conforming
 *     full-size target.
 */
export type TargetSizeException = "spacing" | "inline" | "equivalent";

/** One undersized target, measured in the real engine. */
export interface TargetGeometry {
  /** The selector axe resolved the node to. */
  selector: string;
  /** The node's tag, lowercase — axe's selector can be a bare generated id
   *  (`#_r_15_`), which says nothing about what the node IS. */
  tag: string;
  kind: FindingKind;
  /** Rendered box — what jsdom can never report. */
  widthPx: number;
  heightPx: number;
  html: string;
  /** The `within` containers (from this surface's waivers) the node sits
   *  inside — computed in the page, where the DOM is. */
  containers: string[];
  /** WCAG "Inline" exception candidate: the target sits inside a line of
   *  text (inline display, sibling text in its parent). */
  inlineCandidate: boolean;
  /** WCAG "Equivalent" exception candidate: another element carries the same
   *  accessible name and DOES meet 24×24. Heuristic (aria-label / text
   *  match) — a candidate to eyeball, not a verdict. */
  equivalentCandidate: boolean;
}

/**
 * A documented exception DECISION. The list is the whole policy surface of
 * this harness: a finding is either a violation or it is on this list with
 * the WCAG exception it rests on and a justification someone wrote down.
 * Matching is per surface and per the fields the entry sets — every set
 * field must match, so a waiver cannot leak onto a sibling control:
 *
 *   - `selector`: substring of the node's axe selector;
 *   - `tag`: the node's tag (for nodes whose axe selector is a bare
 *     generated id);
 *   - `within`: an ancestor the node must sit inside (the component that
 *     owns the compact geometry — a nav, a table);
 *   - `maxNodes`: the most nodes the entry may cover on its surface — the
 *     bound that keeps a CLASS waiver from silently swallowing the next
 *     sub-24px control added to the same container.
 */
export interface TargetSizeWaiver {
  /** The `name` of a surface in target-size.spec.ts's SURFACES list. */
  surface: string;
  /** Substring the node's axe selector must carry (e.g. "#var-flag"). */
  selector?: string;
  /** The node's tag (e.g. "input"). */
  tag?: string;
  /** A selector one of the node's ancestors must match. */
  within?: string;
  /** The class bound: more matching nodes than this fails the surface. */
  maxNodes?: number;
  /** The WCAG exception this entry rests on — the decided policy (#2765).
   *  A waiver without one fails the run: an entry that cannot name its
   *  exception is a finding nobody has answered for. */
  decision: TargetSizeException;
  /** Why the named exception honestly applies to this node — the measured
   *  facts (size, spacing, state) the decision stands on. */
  justification: string;
}

/**
 * The decision list — the inventory of sub-24px targets the real engine
 * sees, each answered for under the decided policy (#2765, Q47b: 24px AA
 * floor, WCAG exceptions accepted where honestly met). Every entry names
 * its exception in `decision` and the measured facts in `justification`;
 * an entry missing either fails the run. An entry whose justification
 * stops being true is deleted here, not edited quiet.
 */

// The two chrome classes every AppShell surface carries — declared once,
// spread per surface, because the shell is ONE component and the copies are
// where its justification would drift.
const SHELL_WAIVER_SURFACES = ["runs-list", "launch-form"] as const;

const SKIP_LINK_WAIVER: Omit<TargetSizeWaiver, "surface"> = {
  selector: "not-sr-only",
  tag: "a",
  maxNodes: 1,
  decision: "equivalent",
  justification:
    "The skip link is sr-only at rest (1×1px — visually hidden BY DESIGN) " +
    "and becomes a fixed padded target on keyboard focus — the only state " +
    "in which it can receive pointer input. The focused presentation is " +
    "the conforming full-size target for the same function, so the " +
    "at-rest box rests on SC 2.5.8's Equivalent exception (#2765).",
};

const SIDEBAR_NAV_WAIVER: Omit<TargetSizeWaiver, "surface"> = {
  within: 'nav[aria-label="Primary navigation"]',
  tag: "a",
  maxNodes: 12,
  decision: "spacing",
  justification:
    "The sidebar's primary-nav rows render at 187×16 CSS px (text-xs " +
    "rows, one per studio section) and axe's rule PASSES them through " +
    "the spacing exception: the 24px circle centered on each row meets " +
    "no other target, so SC 2.5.8's Spacing exception is honestly met " +
    "and accepted as the floor (#2765). maxNodes bounds the class: a " +
    "13th sub-24px nav target fails.",
};

export const TARGET_SIZE_WAIVERS: TargetSizeWaiver[] = [
  ...SHELL_WAIVER_SURFACES.map((surface) => ({
    surface,
    ...SKIP_LINK_WAIVER,
  })),
  ...SHELL_WAIVER_SURFACES.map((surface) => ({
    surface,
    ...SIDEBAR_NAV_WAIVER,
  })),
  {
    surface: "runs-list",
    tag: "input",
    within: "table",
    maxNodes: 3,
    decision: "spacing",
    justification:
      "The runs table's row-selection checkboxes are the same h-3.5 w-3.5 " +
      "(14×14 CSS px) Checkbox primitive as VarFieldInput's bool. axe " +
      "passes them through the spacing exception — the 24px circle " +
      "centered on each meets no other target — so Spacing is honestly " +
      "met and accepted as the floor (#2765). Their axe selectors are " +
      "React useId's (`#_r_N_`), which is why this entry matches on " +
      "tag-within-table instead — and maxNodes is the guard against a " +
      "fourth compact input joining unexamined.",
  },
  {
    surface: "runs-list",
    selector: 'title="Filter by',
    maxNodes: 2,
    decision: "spacing",
    justification:
      "The runs table's per-row bot-filter icon buttons render at 20×20 " +
      "CSS px (w-5), under the floor, with the 24px circle around each " +
      "meeting no other target — Spacing exception, accepted as the " +
      "floor (#2765).",
  },
  {
    surface: "runs-list",
    selector: "text-left",
    within: "table",
    maxNodes: 2,
    decision: "spacing",
    justification:
      "The runs table's bot-name open buttons are text rows at ~157×16 " +
      "CSS px — compact table typography, the 24px circle around each " +
      "meeting no other target — Spacing exception, accepted as the " +
      "floor (#2765).",
  },
  {
    surface: "launch-form",
    selector: "#var-flag",
    maxNodes: 1,
    decision: "spacing",
    justification:
      "VarFieldInput's bool checkbox renders at 14×14 CSS px (h-3.5 " +
      "w-3.5), under the SC 2.5.8 24px floor (BotArgsForm and " +
      "AddTaskDialog share the component). The harness measures " +
      "spacing:MET — the 24px circle centered on it meets no other " +
      "target — so the Spacing exception is honestly met and accepted " +
      "as the studio's floor (#2765). Deleting this entry must redden " +
      "the launch-form surface — that is the harness's self-proof that " +
      "it sees what jsdom cannot.",
  },
];

/** The outcome of scanning one surface. */
export interface SurfaceScan {
  /** Undersized targets no waiver covers — the verdict fails on these. */
  findings: TargetGeometry[];
  /** Undersized targets a live waiver covers. */
  waived: { geometry: TargetGeometry; waiver: TargetSizeWaiver }[];
  /** Waivers for this surface that matched no node — they fail the verdict
   *  too, because a dead waiver is a stale claim about the surface. */
  deadWaivers: TargetSizeWaiver[];
  /** Class waivers that covered MORE nodes than their maxNodes — the class
   *  grew past what was decided, so the verdict fails on these too. */
  overgrown: { waiver: TargetSizeWaiver; count: number }[];
}

/** Does this waiver claim this node? Every field the waiver sets must hold. */
function waiverMatches(w: TargetSizeWaiver, g: TargetGeometry): boolean {
  // An entry setting NO matcher would waive the whole surface — refuse it.
  if (!w.selector && !w.tag && !w.within) return false;
  if (w.selector && !g.selector.includes(w.selector)) return false;
  if (w.tag && g.tag !== w.tag) return false;
  if (w.within && !g.containers.includes(w.within)) return false;
  return true;
}

/** One line naming the waiver in a report — whatever fields it set. */
function describeWaiver(w: TargetSizeWaiver): string {
  return [
    w.selector ? `selector ${w.selector}` : null,
    w.tag ? `tag ${w.tag}` : null,
    w.within ? `within ${w.within}` : null,
  ]
    .filter(Boolean)
    .join(", ");
}

interface AxeCheckResult {
  id: string;
}

interface AxeNode {
  target: string[];
  html: string;
  any: AxeCheckResult[];
}

interface AxeRuleResult {
  id: string;
  nodes: AxeNode[];
}

interface AxeRunResult {
  violations: AxeRuleResult[];
  passes: AxeRuleResult[];
  incomplete: AxeRuleResult[];
  inapplicable: AxeRuleResult[];
}

/** Run axe's `target-size` rule over the current page and measure every
 *  sub-24px node it saw, in both classes. */
export async function scanTargetSize(
  page: Page,
  surface: string,
): Promise<SurfaceScan> {
  await page.evaluate(AXE_SOURCE);
  const result = (await page.evaluate(
    // target-size ships DISABLED in axe (`enabled: false` in its metadata —
    // the reason axeHelpers.ts's declaredRuleSwitches exists), and runOnly by
    // rule id does not turn it on: ask for it by id AND enable it, or the
    // run reports the rule as inapplicable and the verdict judges nothing.
    `window.axe.run({
      runOnly: { type: "rule", values: ["target-size"] },
      rules: { "target-size": { enabled: true } },
    })`,
  )) as AxeRunResult;
  // The jsdom failure mode, guarded for the real engine too: a run where the
  // rule touched NO node in ANY bucket judged nothing, and a green verdict on
  // it would be the same lie the ticket exists to close.
  const ruleIn = (bucket: AxeRuleResult[]) =>
    bucket.find((r) => r.id === "target-size");
  const touches = (bucket: AxeRuleResult[]) => ruleIn(bucket)?.nodes.length ?? 0;
  if (
    touches(result.violations) +
      touches(result.passes) +
      touches(result.incomplete) ===
    0
  ) {
    throw new Error(
      `${surface}: axe's target-size rule saw no node on this surface (all ` +
        `buckets empty — inapplicable: ${touches(result.inapplicable)}). The ` +
        `surface did not render what the scanner expects, or the rule is off.`,
    );
  }
  const flagged: { selector: string; kind: FindingKind }[] = [
    ...(ruleIn(result.violations)?.nodes ?? []).map((n) => ({
      selector: n.target.join(" "),
      kind: "axe-violation" as const,
    })),
    // A PASS can still be a sub-24px target: the rule's `target-offset`
    // check is the spacing exception, and a node whose pass NEEDS it is
    // under the floor with empty room around it — the second class the
    // harness judges. axe lists target-offset in `any` whenever it passes,
    // including for nodes that pass on SIZE, so the physical measurement
    // below is the arbiter: only nodes genuinely under the floor are kept.
    ...(ruleIn(result.passes)?.nodes ?? [])
      .filter((n) => n.any.some((c) => c.id === "target-offset"))
      .map((n) => ({
        selector: n.target.join(" "),
        kind: "wcag-spacing-exception" as const,
      })),
  ];
  // Measure in a second pass: axe hands back selectors, the geometry has to
  // come from layout, and only the page has one. SC 2.5.8 reads on both
  // dimensions — a target is undersized when EITHER is under the floor.
  // The within containers the waivers name travel in, so each measured node
  // reports which of them it sits inside.
  const withinSelectors = [
    ...new Set(
      TARGET_SIZE_WAIVERS.filter((w) => w.surface === surface && w.within).map(
        (w) => w.within as string,
      ),
    ),
  ];
  const measured = (await page.evaluate(
    `(${measureNodes.toString()})(${JSON.stringify(flagged)}, ${TARGET_SIZE_FLOOR_PX}, ${JSON.stringify(withinSelectors)})`,
  )) as TargetGeometry[];
  const geometries = measured.filter(
    (g) => g.widthPx < TARGET_SIZE_FLOOR_PX || g.heightPx < TARGET_SIZE_FLOOR_PX,
  );
  const waivers = TARGET_SIZE_WAIVERS.filter((w) => w.surface === surface);
  const matched = new Map<TargetSizeWaiver, number>();
  const findings: TargetGeometry[] = [];
  const waived: SurfaceScan["waived"] = [];
  for (const geometry of geometries) {
    const waiver = waivers.find((w) => waiverMatches(w, geometry));
    if (waiver) {
      matched.set(waiver, (matched.get(waiver) ?? 0) + 1);
      waived.push({ geometry, waiver });
    } else {
      findings.push(geometry);
    }
  }
  const overgrown = [...matched]
    .filter(([w, count]) => w.maxNodes !== undefined && count > w.maxNodes)
    .map(([waiver, count]) => ({ waiver, count }));
  return {
    findings,
    waived,
    deadWaivers: waivers.filter((w) => !matched.has(w)),
    overgrown,
  };
}

/**
 * Runs IN THE PAGE (serialised into page.evaluate): measure each flagged
 * node's box and classify the WCAG exception candidates. Nothing here may
 * close over the module — everything it needs arrives as arguments.
 */
function measureNodes(
  flagged: { selector: string; kind: FindingKind }[],
  floorPx: number,
  withinSelectors: string[],
): TargetGeometry[] {
  const round1 = (n: number) => Math.round(n * 10) / 10;
  const TARGETS =
    "button, a[href], input, select, textarea, summary, [role='button'], [role='link'], [role='checkbox'], [role='switch'], [tabindex]";
  const accessibleNameOf = (el: Element): string =>
    (
      el.getAttribute("aria-label") ??
      el.getAttribute("title") ??
      el.textContent ??
      ""
    ).trim();
  return flagged.map(({ selector, kind }) => {
    const el = document.querySelector(selector);
    if (!el) {
      // axe resolved it a moment ago; a node gone by measure time is a
      // surface that moves under the scanner, reported as a 0×0 finding
      // rather than silently dropped.
      return {
        selector,
        tag: "?",
        kind,
        widthPx: 0,
        heightPx: 0,
        html: "<node gone before measurement>",
        containers: [],
        inlineCandidate: false,
        equivalentCandidate: false,
      };
    }
    const rect = el.getBoundingClientRect();
    const display = getComputedStyle(el).display;
    const parent = el.parentElement;
    // Inline candidate: inline-ish box sharing its parent with real text.
    const inlineCandidate =
      display.startsWith("inline") &&
      !!parent &&
      [...parent.childNodes].some(
        (n) => n.nodeType === Node.TEXT_NODE && (n.textContent ?? "").trim(),
      );
    // Equivalent candidate: a DIFFERENT element with the same accessible
    // name that meets the floor.
    const name = accessibleNameOf(el);
    let equivalentCandidate = false;
    if (name) {
      for (const other of document.querySelectorAll(TARGETS)) {
        if (other === el) continue;
        if (accessibleNameOf(other) !== name) continue;
        const r = other.getBoundingClientRect();
        if (r.width >= floorPx && r.height >= floorPx) {
          equivalentCandidate = true;
          break;
        }
      }
    }
    return {
      selector,
      tag: el.tagName.toLowerCase(),
      kind,
      widthPx: round1(rect.width),
      heightPx: round1(rect.height),
      html: el.outerHTML.replace(/\s+/g, " ").slice(0, 120),
      containers: withinSelectors.filter((w) => el.closest(w) !== null),
      inlineCandidate,
      equivalentCandidate,
    };
  });
}

function formatGeometry(g: TargetGeometry): string {
  const spacing =
    g.kind === "axe-violation"
      ? "spacing:failed (axe's rule evaluates it)"
      : "spacing:MET — the only thing standing between this node and a violation";
  const exceptions = [
    `inline:${g.inlineCandidate ? "candidate" : "no"}`,
    spacing,
    `equivalent:${g.equivalentCandidate ? "candidate" : "no"}`,
  ].join(", ");
  return `\n      ${g.selector} — ${g.widthPx}×${g.heightPx} CSS px (floor ${TARGET_SIZE_FLOOR_PX})\n        ${g.html}\n        exceptions: ${exceptions}`;
}

/**
 * The verdict. Throws a readable report when the surface has an unwaived
 * finding or a dead waiver; returns the scan (its `waived` list is the
 * documented-exception ledger) otherwise.
 */
export async function assertTargetSize(
  page: Page,
  surface: string,
): Promise<SurfaceScan> {
  const scan = await scanTargetSize(page, surface);
  const problems: string[] = [];
  // The waiver list is a DECISION list: an entry that cannot name the WCAG
  // exception it rests on, or say why that exception honestly applies, is a
  // finding nobody answered for — it fails the run like an unwaived one.
  const undecided = TARGET_SIZE_WAIVERS.filter(
    (w) => w.surface === surface && (!w.decision || !w.justification.trim()),
  );
  if (undecided.length > 0) {
    problems.push(
      `waivers missing their WCAG exception decision — every entry must name ` +
        `the exception it rests on (spacing / inline / equivalent) and a ` +
        `non-empty justification (#2765):\n${undecided
          .map((w) => `  - ${describeWaiver(w)}`)
          .join("\n")}`,
    );
  }
  const violations = scan.findings.filter((g) => g.kind === "axe-violation");
  const spacingOnly = scan.findings.filter(
    (g) => g.kind === "wcag-spacing-exception",
  );
  if (violations.length > 0) {
    problems.push(
      `SC 2.5.8 target-size violations no waiver covers:${violations
        .map(formatGeometry)
        .join("")}`,
    );
  }
  if (spacingOnly.length > 0) {
    problems.push(
      `sub-24px targets passing ONLY through WCAG's spacing exception, with no ` +
        `documented decision — the studio's floor accepts that exception only ` +
        `where a waiver records it (#2765):${spacingOnly.map(formatGeometry).join("")}`,
    );
  }
  if (scan.deadWaivers.length > 0) {
    problems.push(
      `waivers that matched nothing — the surface no longer needs them, drop them:\n${scan.deadWaivers
        .map((w) => `  - ${describeWaiver(w)}: ${w.justification}`)
        .join("\n")}`,
    );
  }
  if (scan.overgrown.length > 0) {
    problems.push(
      `class waivers that covered MORE nodes than their declared bound — the class grew, re-decide it:\n${scan.overgrown
        .map(
          ({ waiver, count }) =>
            `  - ${describeWaiver(waiver)} covered ${count} nodes, max ${waiver.maxNodes}: ${waiver.justification}`,
        )
        .join("\n")}`,
    );
  }
  if (problems.length > 0) {
    const waivedNote =
      scan.waived.length > 0
        ? `\n  waived here: ${scan.waived
            .map(
              ({ geometry, waiver }) =>
                `${geometry.selector} (${waiver.justification})`,
            )
            .join("; ")}`
        : "";
    throw new Error(
      `${surface} has ${problems.join("\n\n")}${waivedNote}`,
    );
  }
  return scan;
}
