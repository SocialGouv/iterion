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

export function unnamedInteractives(): UnnamedInteractive[] {
  const out: UnnamedInteractive[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    // The htmlFor↔id associations whose TWO SIDES are the same expression
    // source (the FieldRow useId pattern: htmlFor={inputId} / id={inputId}).
    // A cross-file or runtime-only association stays undecidable here.
    const labelledIds = new Set<string>();
    const idExpressions = new Map<string, ts.Expression>();
    const collect = (n: ts.Node): void => {
      if (ts.isJsxAttribute(n)) {
        const name = n.name.getText();
        if ((name === "htmlFor" || name === "id") && n.initializer && ts.isJsxExpression(n.initializer) && n.initializer.expression) {
          const key = n.initializer.expression.getText();
          if (name === "htmlFor") labelledIds.add(key);
          else idExpressions.set(key, n.initializer.expression);
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
          const idInit = attrs.get("id");
          const idKey =
            idInit && ts.isJsxExpression(idInit) && idInit.expression
              ? idInit.expression.getText()
              : null;
          const idLabelled = idKey !== null && labelledIds.has(idKey);
          const named =
            text.length > 0 ||
            hasExpressionChild(n) ||
            hasAriaLabel ||
            attrs.get("aria-labelledby") !== undefined ||
            hasTitle ||
            idLabelled ||
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
        const nowInLabel = inLabel || (tag === "label" && !!tag);
        ts.forEachChild(n, (c) => visit(c, n === c ? nowInLabel : inLabel));
        return;
      }
      if (ts.isJsxSelfClosingElement(n)) {
        const tag = tagOf(n);
        if (tag && INTERACTIVE.has(tag)) {
          const attrs = attrsOf(n);
          // input[type=hidden] and display:none (the `hidden` utility) are
          // not exposed to assistive tech at all.
          const type = staticText(attrs.get("type") as ts.StringLiteral | undefined);
          const cls = attrs.get("className");
          const classes = (cls && ts.isStringLiteral(cls) && ` ${cls.text} `) || "";
          if (tag === "input" && type === "hidden") return;
          if (classes.includes(" hidden ")) return;
          const named =
            attrs.get("aria-label") !== undefined ||
            attrs.get("aria-labelledby") !== undefined ||
            attrs.get("title") !== undefined ||
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
        return;
      }
      if (ts.isJsxExpression(n) && n.expression) {
        visit(n.expression, inLabel);
        return;
      }
      ts.forEachChild(n, (c) => visit(c, inLabel));
    };

    visit(sf, false);
  }
  return out;
}

function tagOf(el: ts.JsxOpeningElement | ts.JsxSelfClosingElement): string | null {
  const name = el.tagName;
  return ts.isIdentifier(name) || ts.isJsxNamespacedName(name)
    ? (name as { getText(): string }).getText()
    : null;
}
