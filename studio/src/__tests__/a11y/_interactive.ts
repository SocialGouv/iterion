import ts from "typescript";
import { files } from "./_scanner";

// Interactive-element naming scan for the RGAA 11.1 guard (#2591 leg 2):
// every intrinsic interactive element with NONE of the naming mechanisms —
// no visible text, no aria-label, no aria-labelledby, no title, and (for
// form controls) no enclosing <label> — is reported. Placeholder is
// deliberately NOT a naming mechanism (axe 4.13 passes placeholder-only
// inputs outright; the sweep #2590 scan treated it as a lead only).
// NOT a test file — imported by the guard.

export interface UnnamedInteractive {
  path: string;
  line: number;
  tag: string;
  /** The element's own classes, for the waiver entries' greppability. */
  classes: string;
}

const INTERACTIVE = new Set(["button", "a", "input", "textarea", "select"]);

/** The tags an enclosing `<label>` can name. A link is not one of them. */
const LABELABLE = new Set(["button", "input", "textarea", "select"]);

/** A label's offer of its name, consumed by the first labelable descendant. */
interface LabelOffer {
  free: boolean;
}

type Attrs = Map<string, ts.Expression | ts.StringLiteral | undefined>;

const attrsOf = (open: ts.JsxOpeningElement | ts.JsxSelfClosingElement): Attrs => {
  const map: Attrs = new Map();
  for (const a of open.attributes.properties) {
    if (!ts.isJsxAttribute(a)) continue;
    map.set(a.name.getText(), a.initializer ?? undefined);
  }
  return map;
};

const staticText = (init: ts.Expression | undefined): string | null =>
  init && ts.isStringLiteral(init) ? init.text : null;

/** The element's own classes, when they are a literal this scan can read. */
function classesOf(attrs: Attrs): string {
  const cls = attrs.get("className");
  return (cls && ts.isStringLiteral(cls) && cls.text) || "";
}

/**
 * Does this naming attribute actually name? An empty `aria-label` names
 * nothing AND suppresses every fallback, so it is worse than its absence; a
 * runtime value is undecidable here, and the colocated role-name tests are
 * what bite for those, so it counts.
 */
function namesStatically(init: ts.Expression | ts.StringLiteral | undefined): boolean {
  if (init === undefined) return false;
  const lit = staticText(init);
  return lit === null || lit.trim().length > 0;
}

/**
 * An attribute whose written value is `want`, in either spelling: a string
 * literal (`aria-hidden="true"`) or the literal expression React also accepts
 * (`aria-hidden={true}`, `tabIndex={-1}`). Both render the identical DOM, so
 * a reader that took only one of them would report a correct call site.
 */
function writtenAs(attrs: Attrs, name: string, want: string): boolean {
  const init = attrs.get(name);
  if (!init) return false;
  if (ts.isJsxExpression(init) && init.expression) return init.expression.getText() === want;
  return staticText(init) === want;
}

/**
 * The element declares it is out of the accessibility tree AND out of the tab
 * order, the one shape where having no name is not a defect: `aria-hidden`
 * takes it out of the tree whatever the CSS does, and `tabIndex={-1}` is the
 * half that makes the claim true — an aria-hidden element left in the tab
 * order is its own violation (axe aria-hidden-focus). A value saying the
 * opposite, or one this scan cannot read, buys nothing.
 *
 * A utility class is NOT part of the claim. `hidden` is display:none, which
 * keeps a BROWSER from exposing the element, but only while that class
 * applies, and no net that reads the DOM (jsdom loads no stylesheet) can see
 * it — so it says nothing here, and neither does `sr-only`, which exists to
 * keep the element exposed. One predicate for both JSX shapes: the same
 * element written paired must get the same verdict.
 */
function outOfTree(attrs: Attrs): boolean {
  return writtenAs(attrs, "aria-hidden", "true") && writtenAs(attrs, "tabIndex", "-1");
}

/**
 * Does a `<label htmlFor={x}>` in this file point at this element's `id={x}`?
 * Read for a PAIRED element only: the match is file-scoped, so it would also
 * pair a label in one branch with a control in another (see the self-closing
 * branch, where that shape is real).
 */
function labelledById(
  attrs: Map<string, ts.Expression | ts.StringLiteral | undefined>,
  labelledIds: Set<string>,
): boolean {
  const init = attrs.get("id");
  if (!init || !ts.isJsxExpression(init) || !init.expression) return false;
  return labelledIds.has(init.expression.getText());
}

/**
 * Does a `<label>` name anything at all? Its own text, a runtime child, or a
 * name it carries itself — axe's implicit-label check runs the full accname
 * over the label, so an `aria-label` on the label names the control it
 * wraps. A label with none of those names nothing, and a control under it is
 * as unnamed as one standing alone.
 */
function names(label: ts.JsxElement): boolean {
  const attrs = attrsOf(label.openingElement);
  return (
    ownText(label).length > 0 ||
    hasExpressionChild(label) ||
    namesStatically(attrs.get("aria-label")) ||
    namesStatically(attrs.get("aria-labelledby"))
  );
}

/** Concatenated static JSX text of the element's own children (deep). */
function ownText(node: ts.JsxElement): string {
  let out = "";
  const walk = (n: ts.Node): void => {
    if (ts.isJsxText(n)) out += n.getText();
    if (ts.isJsxAttribute(n)) return;
    ts.forEachChild(n, walk);
  };
  for (const child of node.children) walk(child);
  return out.replace(/\s+/g, " ").trim();
}

/** True when any child is a JSX expression — most commonly the runtime
 *  name ({label}, {title}). A source scan cannot decide those names, and
 *  the colocated role-name tests are the net that bites for them; such an
 *  element is therefore not flagged as unnamed. */
function hasExpressionChild(node: ts.JsxElement): boolean {
  let found = false;
  const walk = (n: ts.Node): void => {
    if (found) return;
    if (ts.isJsxExpression(n) && !n.dotDotDotToken) {
      found = true;
      return;
    }
    if (ts.isJsxAttribute(n)) return;
    ts.forEachChild(n, walk);
  };
  for (const child of node.children) walk(child);
  return found;
}

/** The scan over the whole production tree — what the guard judges. */
export function unnamedInteractives(): UnnamedInteractive[] {
  const out: UnnamedInteractive[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    out.push(...unnamedInteractivesIn(path, src));
  }
  return out;
}

/**
 * The same scan over ONE source, by text: the reader this guard rests on is
 * only as good as the shapes it reads, and those are judged on hand-written
 * sources (interactive-scanner.test.ts) rather than on whichever shapes the
 * tree happens to hold today.
 */
export function unnamedInteractivesIn(path: string, src: string): UnnamedInteractive[] {
  const out: UnnamedInteractive[] = [];
  const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  // The htmlFor↔id associations whose TWO SIDES are the same expression
  // source (the FieldRow useId pattern: htmlFor={inputId} / id={inputId}).
  // A cross-file or runtime-only association stays undecidable here.
  const labelledIds = new Set<string>();
  const collect = (n: ts.Node): void => {
    if (ts.isJsxAttribute(n)) {
      const name = n.name.getText();
      if (name === "htmlFor" && n.initializer && ts.isJsxExpression(n.initializer) && n.initializer.expression) {
        labelledIds.add(n.initializer.expression.getText());
      }
    }
    ts.forEachChild(n, collect);
  };
  collect(sf);

  /**
   * The enclosing `<label>` a control may still claim. An HTML label names
   * its FIRST labelable descendant and no other, so the offer is consumed:
   * the second control under one label is as unnamed as one standing alone
   * (a from/to pair under a single "Date range" label is the live shape).
   * Mutable and shared across the label's subtree, which is what makes
   * "first in document order" readable from a recursive walk.
   */
  const claim = (label: LabelOffer | null, tag: string): boolean => {
    if (!label || !label.free || !LABELABLE.has(tag)) return false;
    label.free = false;
    return true;
  };

  const visit = (n: ts.Node, label: LabelOffer | null): void => {
    if (ts.isJsxElement(n)) {
      const tag = tagOf(n.openingElement);
      if (tag && INTERACTIVE.has(tag)) {
        const attrs = attrsOf(n.openingElement);
        // input[type=hidden] is not a labelable element, so it neither needs
        // a name nor spends its label's offer — in either spelling, since
        // the two branches owe each other the same verdict.
        if (tag === "input" && staticText(attrs.get("type")) === "hidden") {
          ts.forEachChild(n, (c) => visit(c, label));
          return;
        }
        const byLabel = claim(label, tag);
        const named =
          outOfTree(attrs) ||
          ownText(n).length > 0 ||
          hasExpressionChild(n) ||
          namesStatically(attrs.get("aria-label")) ||
          namesStatically(attrs.get("aria-labelledby")) ||
          namesStatically(attrs.get("title")) ||
          labelledById(attrs, labelledIds) ||
          byLabel;
        if (!named) {
          out.push({
            path,
            line: sf.getLineAndCharacterOfPosition(n.getStart()).line + 1,
            tag,
            classes: classesOf(attrs),
          });
        }
      }
      // forEachChild only ever hands back a CHILD, so the children of a
      // <label> are exactly the nodes that inherit its offer. A label with no
      // text and no runtime child names nothing, and a control it wraps is as
      // unnamed as one standing alone.
      const inner =
        tag === "label" && names(n)
          ? { free: true }
          : label;
      ts.forEachChild(n, (c) => visit(c, inner));
      return;
    }
    if (ts.isJsxSelfClosingElement(n)) {
      const tag = tagOf(n);
      if (tag && INTERACTIVE.has(tag)) {
        const attrs = attrsOf(n);
        // input[type=hidden] is not a labelable element, so it neither needs
        // a name nor spends its label's offer.
        if (tag === "input" && staticText(attrs.get("type")) === "hidden") return;
        // Claimed BEFORE anything else can return or short-circuit: a
        // control that is named another way still spends the offer, or the
        // next bare sibling would claim a label HTML never gave it. The
        // paired branch above does the same, and the two must agree.
        const byLabel = claim(label, tag);
        // HiddenFileInput's shape: a picker a button drives, declared out of
        // the tree and out of the tab order.
        if (outOfTree(attrs)) return;
        const named =
          namesStatically(attrs.get("aria-label")) ||
          namesStatically(attrs.get("aria-labelledby")) ||
          namesStatically(attrs.get("title")) ||
          // Deliberately NOT the htmlFor/id pair: the association is
          // FILE-scoped, and a primitive with a labelled and an unlabelled
          // branch (Checkbox, Radio) would have its unlabelled control named
          // by the other branch's htmlFor. A false positive here costs a
          // waiver naming the consumer's test; a false negative costs the
          // class the guard exists for.
          byLabel;
        if (!named) {
          out.push({
            path,
            line: sf.getLineAndCharacterOfPosition(n.getStart()).line + 1,
            tag,
            classes: classesOf(attrs),
          });
        }
      }
      // A component's JSX-valued props (`footer={<button />}`) live in its
      // attributes: walking them is the only way an element handed over as a
      // prop is read at all.
      ts.forEachChild(n, (c) => visit(c, label));
      return;
    }
    // Two arms of a conditional are two renders, never one: each gets the
    // offer whole, and it is spent only if both arms spend it. Without the
    // fork, `{m ? <input/> : <input/>}` under one label reports the second
    // arm, which the first never rendered beside.
    if (label && ts.isConditionalExpression(n)) {
      // The condition is one render, not two: it keeps the shared offer.
      visit(n.condition, label);
      const spent = [n.whenTrue, n.whenFalse].map((arm) => {
        const fork: LabelOffer = { free: label.free };
        visit(arm, fork);
        return !fork.free;
      });
      // Spent as soon as ANY arm spends it: what FOLLOWS the conditional is
      // unnamed in that render, and a defect in one render is a defect. The
      // `{m && <input/>}` spelling goes through the shared walk below and
      // reaches the same verdict, which is the point.
      label.free = label.free && !spent.some(Boolean);
      return;
    }
    if (ts.isJsxExpression(n) && n.expression) {
      visit(n.expression, label);
      return;
    }
    ts.forEachChild(n, (c) => visit(c, label));
  };

  visit(sf, null);
  return out;
}

function tagOf(el: ts.JsxOpeningElement | ts.JsxSelfClosingElement): string | null {
  const name = el.tagName;
  return ts.isIdentifier(name) || ts.isJsxNamespacedName(name)
    ? (name as { getText(): string }).getText()
    : null;
}
