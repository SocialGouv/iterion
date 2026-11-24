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

const attributeOf = (
  el: ts.JsxOpeningElement | ts.JsxSelfClosingElement,
  name: string,
): ts.JsxAttribute | undefined =>
  el.attributes.properties.find(
    (a): a is ts.JsxAttribute => ts.isJsxAttribute(a) && a.name.getText() === name,
  );

const hasAttribute = (
  el: ts.JsxOpeningElement | ts.JsxSelfClosingElement,
  name: string,
): boolean => attributeOf(el, name) !== undefined;

/** `focusable={false}` — the literal, not an expression: a nested clamp's
 *  opt-out is a greppable fact written at its call site, and a dynamic one
 *  could resolve true and put the stop back. */
const hasLiteralFalseAttr = (
  el: ts.JsxOpeningElement | ts.JsxSelfClosingElement,
  name: string,
): boolean => {
  const a = attributeOf(el, name);
  if (!a?.initializer || !ts.isJsxExpression(a.initializer)) return false;
  return a.initializer.expression?.kind === ts.SyntaxKind.FalseKeyword;
};

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

export interface ClampNestedHit {
  path: string;
  line: number;
  /** The call site carries focusable={false}: the control around it is the
   *  one tab stop and its name carries the text. */
  optedOut: boolean;
}

/** Every <ClampText> nested in the JSX children of an intrinsic <button> or
 *  <a href>, with whether its call site opted out of the focus stop. A
 *  focusable block inside a control is a second tab stop for one control
 *  (#2803), so a nested clamp must carry focusable={false} — ClampText is
 *  focusable by default, and only the literal opt-out keeps the control
 *  alone in the tab order.
 *
 *  Descends through intrinsic elements and the usual child shapes
 *  (fragments, conditionals, &&/|| arms, arrays, map callbacks); stops at
 *  every other component, whose render is its own business — a ClampText a
 *  component hides is that component's to keep out of the tab order. */
export function clampTextsNestedInControls(): ClampNestedHit[] {
  const out: ClampNestedHit[] = [];
  const push = (el: ts.JsxOpeningElement | ts.JsxSelfClosingElement, node: ts.Node, sf: ts.SourceFile, path: string): void => {
    out.push({
      path,
      line: sf.getLineAndCharacterOfPosition(node.getStart()).line + 1,
      optedOut: hasLiteralFalseAttr(el, "focusable"),
    });
  };
  const collect = (node: ts.Node | undefined, sf: ts.SourceFile, path: string): void => {
    if (!node) return;
    if (ts.isJsxExpression(node)) {
      if (node.dotDotDotToken) return;
      return collect(node.expression, sf, path);
    }
    if (ts.isJsxFragment(node)) {
      for (const child of node.children) collect(child, sf, path);
      return;
    }
    if (ts.isJsxElement(node)) {
      const tag = tagOf(node.openingElement);
      if (tag === "ClampText") {
        push(node.openingElement, node, sf, path);
        return;
      }
      // An intrinsic element's children are still this control's DOM; a
      // component's are not.
      if (tag && /^[a-z]/.test(tag)) {
        for (const child of node.children) collect(child, sf, path);
      }
      return;
    }
    if (ts.isJsxSelfClosingElement(node)) {
      if (tagOf(node) === "ClampText") push(node, node, sf, path);
      return;
    }
    if (
      ts.isParenthesizedExpression(node) ||
      ts.isAsExpression(node) ||
      ts.isSatisfiesExpression(node) ||
      ts.isNonNullExpression(node)
    ) {
      return collect(node.expression, sf, path);
    }
    if (ts.isConditionalExpression(node)) {
      collect(node.whenTrue, sf, path);
      collect(node.whenFalse, sf, path);
      return;
    }
    if (ts.isBinaryExpression(node)) {
      const op = node.operatorToken.kind;
      if (
        op === ts.SyntaxKind.AmpersandAmpersandToken ||
        op === ts.SyntaxKind.BarBarToken ||
        op === ts.SyntaxKind.QuestionQuestionToken
      ) {
        collect(node.left, sf, path);
        collect(node.right, sf, path);
      }
      return;
    }
    if (ts.isArrayLiteralExpression(node)) {
      for (const el of node.elements) collect(el, sf, path);
      return;
    }
    if (ts.isArrowFunction(node) || ts.isFunctionExpression(node)) {
      return collect(node.body, sf, path);
    }
    if (ts.isCallExpression(node)) {
      for (const arg of node.arguments) collect(arg, sf, path);
      return;
    }
  };
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    const visit = (n: ts.Node): void => {
      if (ts.isJsxElement(n) || ts.isJsxSelfClosingElement(n)) {
        const el = ts.isJsxElement(n) ? n.openingElement : n;
        const tag = tagOf(el);
        // <button> is always a control; an <a> is one when it navigates.
        if (tag === "button" || (tag === "a" && hasAttribute(el, "href"))) {
          if (ts.isJsxElement(n)) for (const child of n.children) collect(child, sf, path);
          return;
        }
      }
      ts.forEachChild(n, visit);
    };
    visit(sf);
  }
  return out;
}
