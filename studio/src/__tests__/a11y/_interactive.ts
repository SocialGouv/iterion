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

const attrsOf = (open: ts.JsxOpeningElement | ts.JsxSelfClosingElement) => {
  const map = new Map<string, ts.Expression | ts.StringLiteral | undefined>();
  for (const a of open.attributes.properties) {
    if (!ts.isJsxAttribute(a)) continue;
    map.set(a.name.getText(), a.initializer ?? undefined);
  }
  return map;
};

const staticText = (init: ts.Expression | undefined): string | null =>
  init && ts.isStringLiteral(init) ? init.text : null;

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
      if ((name === "htmlFor" || name === "id") && n.initializer && ts.isJsxExpression(n.initializer) && n.initializer.expression) {
        const key = n.initializer.expression.getText();
        if (name === "htmlFor") labelledIds.add(key);
      }
    }
    ts.forEachChild(n, collect);
  };
  collect(sf);

  const visit = (n: ts.Node, inLabel: boolean): void => {
    if (ts.isJsxElement(n)) {
      const tag = tagOf(n.openingElement);
      if (tag && INTERACTIVE.has(tag)) {
        const attrs = attrsOf(n.openingElement);
        const text = ownText(n);
        const ariaLabel = attrs.get("aria-label");
        const title = attrs.get("title");
        const hasAriaLabel = ariaLabel !== undefined;
        const hasTitle = title !== undefined;
        const named =
          text.length > 0 ||
          hasExpressionChild(n) ||
          hasAriaLabel ||
          attrs.get("aria-labelledby") !== undefined ||
          hasTitle ||
          labelledById(attrs, labelledIds) ||
          (inLabel && (tag === "input" || tag === "textarea" || tag === "select"));
        if (!named) {
          const cls = attrs.get("className");
          out.push({
            path,
            line: sf.getLineAndCharacterOfPosition(n.getStart()).line + 1,
            tag,
            classes: (cls && ts.isStringLiteral(cls) && cls.text) || "",
          });
        }
      }
      // forEachChild only ever hands back a CHILD, so the children of a
      // <label> are exactly the nodes that inherit inLabel.
      const nowInLabel = inLabel || tag === "label";
      ts.forEachChild(n, (c) => visit(c, nowInLabel));
      return;
    }
    if (ts.isJsxSelfClosingElement(n)) {
      const tag = tagOf(n);
      if (tag && INTERACTIVE.has(tag)) {
        const attrs = attrsOf(n);
        // input[type=hidden] carries no control at all.
        const type = staticText(attrs.get("type") as ts.StringLiteral | undefined);
        const cls = attrs.get("className");
        const classes = (cls && ts.isStringLiteral(cls) && ` ${cls.text} `) || "";
        if (tag === "input" && type === "hidden") return;
        // The `hidden` utility is display:none, which a BROWSER keeps out of
        // the a11y tree — but only while that class applies, and no net that
        // reads the DOM (jsdom loads no stylesheet) can see it. So the
        // exemption costs an explicit `aria-hidden="true"`: the element has
        // to SAY it is out of the tree, and a value that says the opposite
        // (or one this scan cannot read) buys nothing. HiddenFileInput is
        // that shape for a picker a button drives.
        if (classes.includes(" hidden ") && staticText(attrs.get("aria-hidden")) === "true") return;
        const named =
          attrs.get("aria-label") !== undefined ||
          attrs.get("aria-labelledby") !== undefined ||
          attrs.get("title") !== undefined ||
          // Deliberately NOT the htmlFor/id pair: the association is
          // FILE-scoped, and a primitive with a labelled and an unlabelled
          // branch (Checkbox, Radio) would have its unlabelled control named
          // by the other branch's htmlFor. A false positive here costs a
          // waiver naming the consumer's test; a false negative costs the
          // class the guard exists for.
          (inLabel && (tag === "input" || tag === "textarea" || tag === "select"));
        if (!named) {
          out.push({
            path,
            line: sf.getLineAndCharacterOfPosition(n.getStart()).line + 1,
            tag,
            classes: (cls && ts.isStringLiteral(cls) && cls.text) || "",
          });
        }
      }
      return;
    }
    if (ts.isJsxExpression(n) && n.expression) {
      visit(n.expression, inLabel);
      return;
    }
    ts.forEachChild(n, (c) => visit(c, inLabel));
  };

  visit(sf, false);
  return out;
}

function tagOf(el: ts.JsxOpeningElement | ts.JsxSelfClosingElement): string | null {
  const name = el.tagName;
  return ts.isIdentifier(name) || ts.isJsxNamespacedName(name)
    ? (name as { getText(): string }).getText()
    : null;
}
