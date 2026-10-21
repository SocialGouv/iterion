import ts from "typescript";
import { files } from "./_scanner";

// JSX-aware companion of _scanner.ts for the class-level guards: every JSX
// element with a `className`, read as the VARIANTS its static text can take
// — a string literal; every combination of a template literal's quasis and
// the branches of its `${…}`; both arms of a ternary; `cond && "x"` as
// present-or-absent; the arguments and the callee chain of a call
// (`[…].filter(Boolean).join(" ")`, `cn()` / `clsx()`); an object literal
// as optional keys (`clsx({ "opacity-70": cond })`) or, behind an index
// (`variantClass[variant]`), as one-of-its-values; an identifier resolved
// to its `const` in the same file. A pairing rule ("white text needs a
// solid accent background") is judged per variant, so two classes from
// different arms never meet; a presence rule reads the union. Variants
// are de-duplicated as they multiply (a prop that only toggles a class
// the element already has adds none), and past MAX_VARIANTS distinct
// ones the product collapses to the union (`collapsed`, which a guard
// refuses). A raw-text regex over `className="…"` saw one shape of these
// (and an `=>` before `className` ended its `[^>]*` early).
// NOT a test file — imported by the guards.

export interface JsxClassed {
  path: string;
  line: number;
  /** The tag as written: `input`, `span`, `Button`, `RT.List`. */
  tag: string;
  /** Every static shape the className can take (at least one). */
  variants: string[];
  /** The union of the variants, for presence rules. */
  classes: string;
  /** True when the variant product overflowed and `variants` collapsed to the union. */
  collapsed: boolean;
  /** A numeric `opacity` in an inline `style={{…}}` literal, when < 1. */
  styleOpacity?: number;
  /** The static `type` attribute of an <input>, when written as a string. */
  inputType?: string;
}

const MAX_VARIANTS = 512;

const uniq = (xs: Iterable<string>): string[] => [...new Set(xs)];

function resolver(sf: ts.SourceFile): (name: string) => ts.Expression | undefined {
  const decls = new Map<string, ts.Expression>();
  const visit = (n: ts.Node): void => {
    if (ts.isVariableDeclaration(n) && ts.isIdentifier(n.name) && n.initializer && !decls.has(n.name.text)) {
      decls.set(n.name.text, n.initializer);
    }
    ts.forEachChild(n, visit);
  };
  visit(sf);
  return (name) => decls.get(name);
}

/** A leaf an expression can evaluate to, with the consts resolved on the way (cycle guard). */
type Leaf = [ts.Expression, ReadonlySet<string>];

class Variants {
  collapsed = false;
  constructor(private readonly lookup: (n: string) => ts.Expression | undefined) {}

  /** Every pairing of the two sides, duplicates folded (two arms yielding the same text are one variant); the cap counts distinct variants. */
  product(a: string[], b: string[]): string[] {
    const ua = uniq(a);
    const ub = uniq(b);
    const out = uniq(ua.flatMap((x) => ub.map((y) => `${x} ${y}`)));
    if (out.length > MAX_VARIANTS) {
      this.collapsed = true;
      return [`${ua.join(" ")} ${ub.join(" ")}`];
    }
    return out;
  }

  unwrap(e: ts.Expression): ts.Expression {
    let x = e;
    while (ts.isParenthesizedExpression(x) || ts.isAsExpression(x) || ts.isSatisfiesExpression(x) || ts.isNonNullExpression(x)) {
      x = x.expression;
    }
    return x;
  }

  /**
   * The leaves an expression can evaluate to (one of): an identifier
   * follows its `const`, a conditional or `||`/`??` yields both sides,
   * `obj.key` / `obj["key"]` the matching property of every object literal
   * `obj` can be, `obj[k]` with a dynamic `k` every property. The set of
   * consts followed travels with each leaf — per path, so a const one
   * branch resolved is still resolved by its sibling.
   */
  values(e: ts.Expression | undefined, seen: ReadonlySet<string>): Leaf[] {
    if (!e) return [];
    const x = this.unwrap(e);
    if (ts.isIdentifier(x)) {
      if (seen.has(x.text)) return [];
      const d = this.lookup(x.text);
      return d ? this.values(d, new Set(seen).add(x.text)) : [];
    }
    if (ts.isConditionalExpression(x)) return [...this.values(x.whenTrue, seen), ...this.values(x.whenFalse, seen)];
    if (ts.isBinaryExpression(x)) {
      const op = x.operatorToken.kind;
      if (op === ts.SyntaxKind.BarBarToken || op === ts.SyntaxKind.QuestionQuestionToken) {
        return [...this.values(x.left, seen), ...this.values(x.right, seen)];
      }
      return [[x, seen]];
    }
    if (ts.isPropertyAccessExpression(x)) return this.pick(x.expression, x.name.text, seen);
    if (ts.isElementAccessExpression(x)) {
      const arg = this.unwrap(x.argumentExpression);
      const key = ts.isStringLiteral(arg) || ts.isNoSubstitutionTemplateLiteral(arg) ? arg.text : undefined;
      return this.pick(x.expression, key, seen);
    }
    return [[x, seen]];
  }

  /** The leaves of `obj.key` — every property when `key` is dynamic. */
  pick(obj: ts.Expression, key: string | undefined, seen: ReadonlySet<string>): Leaf[] {
    return this.values(obj, seen).flatMap(([c, path]) => {
      if (!ts.isObjectLiteralExpression(c)) return [];
      return c.properties.flatMap((p): Leaf[] => {
        if (ts.isShorthandPropertyAssignment(p)) {
          return key === undefined || p.name.text === key ? this.values(p.name, path) : [];
        }
        if (!ts.isPropertyAssignment(p)) return [];
        const name =
          ts.isStringLiteral(p.name) || ts.isIdentifier(p.name) || ts.isNumericLiteral(p.name) ? p.name.text : undefined;
        if (key !== undefined && name !== key) return [];
        return this.values(p.initializer, path);
      });
    });
  }

  of(e: ts.Expression | undefined, seen: ReadonlySet<string>): string[] {
    if (!e) return [""];
    const x = this.unwrap(e);
    const v = (y: ts.Expression | undefined) => this.of(y, seen);
    if (ts.isStringLiteral(x) || ts.isNoSubstitutionTemplateLiteral(x)) return [x.text];
    if (ts.isTemplateExpression(x)) {
      let acc = [x.head.text];
      for (const s of x.templateSpans) {
        acc = this.product(acc, v(s.expression)).map((t) => `${t} ${s.literal.text}`);
      }
      return acc;
    }
    if (ts.isConditionalExpression(x)) return [...v(x.whenTrue), ...v(x.whenFalse)];
    if (ts.isBinaryExpression(x)) {
      const op = x.operatorToken.kind;
      if (op === ts.SyntaxKind.AmpersandAmpersandToken) return ["", ...v(x.right)];
      if (op === ts.SyntaxKind.BarBarToken || op === ts.SyntaxKind.QuestionQuestionToken) return [...v(x.left), ...v(x.right)];
      return this.product(v(x.left), v(x.right));
    }
    if (ts.isCallExpression(x)) {
      let acc = ts.isPropertyAccessExpression(x.expression) ? v(x.expression.expression) : [""];
      for (const a of x.arguments) acc = this.product(acc, v(a));
      return acc;
    }
    if (ts.isArrayLiteralExpression(x)) {
      let acc = [""];
      for (const el of x.elements) acc = this.product(acc, v(el));
      return acc;
    }
    if (ts.isObjectLiteralExpression(x)) {
      // clsx shape: every key is an optional class.
      let acc = [""];
      for (const p of x.properties) {
        if (!ts.isPropertyAssignment(p)) continue;
        const key = ts.isStringLiteral(p.name) || ts.isIdentifier(p.name) ? p.name.text : "";
        acc = this.product(acc, ["", key]);
      }
      return acc;
    }
    if (ts.isIdentifier(x) || ts.isPropertyAccessExpression(x) || ts.isElementAccessExpression(x)) {
      const leaves = this.values(x, seen);
      if (!leaves.length) return [""];
      // A map value that is itself an object (`TONE[k]` without a `.text`)
      // is not a class string; an identifier naming a clsx object is.
      const out = leaves.flatMap(([leaf, path]) =>
        ts.isObjectLiteralExpression(leaf) && !ts.isIdentifier(x) ? [""] : this.of(leaf, path),
      );
      return out.length ? out : [""];
    }
    return [""];
  }
}

function inlineOpacity(e: ts.Expression | undefined): number | undefined {
  if (!e) return undefined;
  let found: number | undefined;
  const visit = (n: ts.Node): void => {
    if (
      ts.isPropertyAssignment(n) &&
      (ts.isIdentifier(n.name) || ts.isStringLiteral(n.name)) &&
      n.name.text === "opacity" &&
      ts.isNumericLiteral(n.initializer)
    ) {
      const value = Number(n.initializer.text);
      if (value < 1) found = value;
    }
    ts.forEachChild(n, visit);
  };
  visit(e);
  return found;
}

let cache: JsxClassed[] | undefined;

/** Every JSX element carrying a className (or an inline opacity / input type) in one source. */
export function classedIn(path: string, src: string): JsxClassed[] {
  const out: JsxClassed[] = [];
  const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const lookup = resolver(sf);
  const visit = (n: ts.Node): void => {
    if (ts.isJsxOpeningElement(n) || ts.isJsxSelfClosingElement(n)) {
      let variants: string[] | undefined;
      let collapsed = false;
      let styleOpacity: number | undefined;
      let inputType: string | undefined;
      for (const a of n.attributes.properties) {
        if (!ts.isJsxAttribute(a) || !a.initializer) continue;
        const name = a.name.getText();
        const init = a.initializer;
        if (name === "className") {
          if (ts.isStringLiteral(init)) {
            variants = [init.text];
          } else if (ts.isJsxExpression(init)) {
            const reader = new Variants(lookup);
            variants = reader.of(init.expression, new Set());
            collapsed = reader.collapsed;
          } else {
            variants = [""];
          }
        } else if (name === "style" && ts.isJsxExpression(init)) {
          styleOpacity = inlineOpacity(init.expression);
        } else if (name === "type" && ts.isStringLiteral(init)) {
          inputType = init.text;
        }
      }
      if (variants !== undefined || styleOpacity !== undefined || inputType !== undefined) {
        const vs = variants ?? [""];
        out.push({
          path,
          line: sf.getLineAndCharacterOfPosition(n.getStart()).line + 1,
          tag: n.tagName.getText(),
          variants: vs,
          classes: vs.join(" "),
          collapsed,
          styleOpacity,
          inputType,
        });
      }
    }
    ts.forEachChild(n, visit);
  };
  visit(sf);
  return out;
}

/** Every JSX element carrying a className (or an inline opacity), over the scanner's file set. */
export function jsxClassed(): JsxClassed[] {
  if (cache) return cache;
  const out: JsxClassed[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    out.push(...classedIn(path, src));
  }
  cache = out;
  return out;
}

/** Intrinsic HTML tags (lowercase), not components. */
export const isIntrinsic = (tag: string): boolean => /^[a-z]/.test(tag);

export const hit = (e: JsxClassed, variant?: string): string =>
  `${e.path}:${e.line}  <${e.tag}> ${e.styleOpacity !== undefined ? `style opacity ${e.styleOpacity} ` : ""}${(variant ?? e.classes).trim().slice(0, 90)}`;
