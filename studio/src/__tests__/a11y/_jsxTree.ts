import ts from "typescript";
import { files } from "./_scanner";

// JSX-tree walk for the content-model guards (jsx-content.test.ts):
// descends through the JSX CHILDREN of an element — fragments,
// conditional and && arms, arrays, map callbacks — but stops at any
// component boundary, because what a component renders is its own
// business. NOT a test file — imported by the guards.

export interface JsxHit {
  path: string;
  line: number;
  tag: string;
}

const tagOf = (el: ts.JsxOpeningElement | ts.JsxSelfClosingElement): string | null =>
  ts.isIdentifier(el.tagName) ? el.tagName.text : null;

function scanChild(node: ts.Node | undefined, hit: ts.Node[]): void {
  if (!node) return;
  if (ts.isJsxExpression(node)) {
    if (node.dotDotDotToken) return;
    return scanChild(node.expression, hit);
  }
  if (ts.isJsxElement(node)) {
    const tag = tagOf(node.openingElement);
    if (tag === "div") {
      hit.push(node);
      return;
    }
    // An intrinsic element's children are still this element's DOM;
    // a component's are not.
    if (tag && /^[a-z]/.test(tag)) {
      for (const child of node.children) scanChild(child, hit);
    }
    return;
  }
  if (ts.isJsxSelfClosingElement(node)) {
    if (tagOf(node) === "div") hit.push(node);
    return;
  }
  if (
    ts.isParenthesizedExpression(node) ||
    ts.isAsExpression(node) ||
    ts.isSatisfiesExpression(node) ||
    ts.isNonNullExpression(node)
  ) {
    return scanChild(node.expression, hit);
  }
  if (ts.isConditionalExpression(node)) {
    scanChild(node.whenTrue, hit);
    scanChild(node.whenFalse, hit);
    return;
  }
  if (ts.isBinaryExpression(node)) {
    const op = node.operatorToken.kind;
    if (
      op === ts.SyntaxKind.AmpersandAmpersandToken ||
      op === ts.SyntaxKind.BarBarToken ||
      op === ts.SyntaxKind.QuestionQuestionToken
    ) {
      scanChild(node.left, hit);
      scanChild(node.right, hit);
    }
    return;
  }
  if (ts.isArrayLiteralExpression(node)) {
    for (const el of node.elements) scanChild(el, hit);
    return;
  }
  if (ts.isArrowFunction(node) || ts.isFunctionExpression(node)) {
    return scanChild(node.body, hit);
  }
  if (ts.isCallExpression(node)) {
    for (const arg of node.arguments) scanChild(arg, hit);
    return;
  }
}

/** Every intrinsic <button> element with an intrinsic <div> descendant
 *  (a button accepts phrasing content only — RGAA 8.2), across the
 *  scanner's file set. */
export function buttonsWithDivDescendants(): JsxHit[] {
  const out: JsxHit[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    const visit = (n: ts.Node): void => {
      if (ts.isJsxElement(n) && tagOf(n.openingElement) === "button") {
        const hit: ts.Node[] = [];
        for (const child of n.children) scanChild(child, hit);
        for (const h of hit) {
          out.push({
            path,
            line: sf.getLineAndCharacterOfPosition(h.getStart()).line + 1,
            tag: "button",
          });
        }
      }
      ts.forEachChild(n, visit);
    };
    visit(sf);
  }
  return out;
}

/** Every element whose tag ends in "Icon" (the Radix icon convention)
 *  carrying an aria-label WITHOUT role="img" — the name and the image
 *  role must sit on the same element, or assistive tech drops the name
 *  (RGAA 1.1). */
export function iconsWithLabelMissingRole(): JsxHit[] {
  const out: JsxHit[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    const visit = (n: ts.Node): void => {
      if (ts.isJsxSelfClosingElement(n) || ts.isJsxOpeningElement(n)) {
        const tag = tagOf(n);
        if (tag && /Icon$/.test(tag)) {
          let hasLabel = false;
          let hasRole = false;
          for (const a of n.attributes.properties) {
            if (!ts.isJsxAttribute(a)) continue;
            const name = a.name.getText();
            if (name === "aria-label") hasLabel = true;
            if (name === "role") hasRole = true;
          }
          if (hasLabel && !hasRole) {
            out.push({
              path,
              line: sf.getLineAndCharacterOfPosition(n.getStart()).line + 1,
              tag,
            });
          }
        }
      }
      ts.forEachChild(n, visit);
    };
    visit(sf);
  }
  return out;
}
