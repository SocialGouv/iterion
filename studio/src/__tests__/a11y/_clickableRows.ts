import ts from "typescript";
import { files } from "./_scanner";
import { isIntrinsic } from "./_jsx";

// The class-level scan behind the clickable-row guard (#2766): every element
// that spreads `clickableRowProps` or `pointerRowProps`, read with its DOM
// descendants, so the pair's two rules are judged at every call site instead
// of one surface at a time.
//
//   - a `clickableRowProps` row IS the control (role=button + tabindex), so
//     it must hold NOTHING focusable — a control inside a control is axe
//     `nested-interactive` (WCAG 4.1.2), and the row's Enter races the inner
//     control's;
//   - a `pointerRowProps` row is a pointer convenience, legal only because a
//     real control inside it carries the keyboard path (RGAA 7.3) — a row
//     with nothing focusable inside is mouse-only;
//   - neither row writes its own `role` / `tabIndex` / `onClick` / `onKeyDown`
//     beside the spread: a later JSX prop WINS, so the helper's handler
//     silently stops running (and that is how one row came to open on Enter
//     and select on click).
//
// The hard part, and why this is not a regex: at most call sites the
// focusable descendant is a COMPONENT (`<Button>`, `<BotAvatar>`), so the
// scan follows it to its own definition through the importing file's import
// list. A component it cannot follow — an external package, a re-export, a
// qualified name — is reported as UNRESOLVED rather than assumed harmless,
// which is what makes the guard's waiver list a claim somebody checked.
// NOT a test file — imported by the guard.

export type HelperName = "clickableRowProps" | "pointerRowProps";

export interface RowHit {
  path: string;
  line: number;
  /** The tag the helper is spread on. */
  tag: string;
  helper: HelperName;
  /** What the scan found there, for the failure message and the waivers. */
  detail: string;
}

export interface RowScan {
  /** Every call site of either helper — the scan's own non-vacuity. */
  sites: RowHit[];
  /** A `clickableRowProps` row holding a focusable descendant. */
  nested: RowHit[];
  /** A `pointerRowProps` row holding nothing focusable: mouse-only. */
  mouseOnly: RowHit[];
  /** A row whose own prop shadows what the helper's spread provides. */
  shadowed: RowHit[];
  /** A descendant the scan could not follow: decide it, never assume it. */
  unresolved: RowHit[];
}

const HELPERS: HelperName[] = ["clickableRowProps", "pointerRowProps"];

/** Props that would shadow what the helper's spread provides. */
const SHADOWING = ["role", "tabIndex", "onClick", "onKeyDown"];

type Attrs = Map<string, ts.JsxAttributeValue | undefined>;

const attrsOf = (open: ts.JsxOpeningElement | ts.JsxSelfClosingElement): Attrs => {
  const map: Attrs = new Map();
  for (const a of open.attributes.properties) {
    if (!ts.isJsxAttribute(a)) continue;
    map.set(a.name.getText(), a.initializer ?? undefined);
  }
  return map;
};

/** The attribute's written value, when it is a literal this scan can read. */
function written(attrs: Attrs, name: string): string | null {
  const init = attrs.get(name);
  if (!init) return null;
  if (ts.isStringLiteral(init)) return init.text;
  if (ts.isJsxExpression(init) && init.expression) return init.expression.getText();
  return null;
}

/** A tabIndex that puts the element in the tab order (0 or more). */
function tabbable(attrs: Attrs): boolean {
  const value = written(attrs, "tabIndex");
  if (value === null) return false;
  const n = Number(value);
  return Number.isFinite(n) && n >= 0;
}

/** Intrinsic tags that are focusable as written, given their attributes. */
function intrinsicFocusable(tag: string, attrs: Attrs): boolean {
  switch (tag) {
    case "button":
    case "select":
    case "textarea":
    case "summary":
      return true;
    case "a":
      return attrs.has("href");
    case "input":
      // A hidden input is not focusable; every other type is. A utility
      // class says nothing here: jsdom loads no stylesheet, and `sr-only`
      // exists precisely to keep the control exposed and focusable.
      return written(attrs, "type") !== "hidden";
    default:
      return false;
  }
}

const tagOf = (el: ts.JsxOpeningElement | ts.JsxSelfClosingElement): string =>
  el.tagName.getText();

/** Which of the two helpers this element spreads, if either. */
function helperOf(open: ts.JsxOpeningElement | ts.JsxSelfClosingElement): HelperName | null {
  for (const a of open.attributes.properties) {
    if (!ts.isJsxSpreadAttribute(a)) continue;
    const e = a.expression;
    if (!ts.isCallExpression(e)) continue;
    const callee = e.expression.getText();
    const found = HELPERS.find((h) => callee === h);
    if (found) return found;
  }
  return null;
}

/** `/src/a/b/../c` → `/src/a/c`, so a relative import resolves. */
function normalise(path: string): string {
  const out: string[] = [];
  for (const part of path.split("/")) {
    if (part === "" || part === ".") continue;
    if (part === "..") out.pop();
    else out.push(part);
  }
  return "/" + out.join("/");
}

interface Imported {
  /** The scanned file the name comes from, or null when external. */
  file: string | null;
  /** The name inside that file: the exported name, or null for a default. */
  exported: string | null;
}

/**
 * The scan over one file set. Taking the sources as an argument is what lets
 * the rules be exercised on sources written for the purpose (and a synthetic
 * import resolve against its synthetic module), the shape `_interactive.ts`
 * already uses for its scanner's tests.
 */
export function scanRows(sources: Iterable<[string, string]>): RowScan {
  const byPath = new Map(sources);
  const parsedCache = new Map<string, ts.SourceFile>();
  const importsCache = new Map<string, Map<string, Imported>>();

  const parsed = (path: string, src: string): ts.SourceFile => {
    const hit = parsedCache.get(path);
    if (hit) return hit;
    const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    parsedCache.set(path, sf);
    return sf;
  };

  /** The scanned file a module specifier names, or null when external. */
  const moduleFile = (fromPath: string, specifier: string): string | null => {
    let base: string;
    if (specifier.startsWith("@/")) base = normalise("/src/" + specifier.slice(2));
    else if (specifier.startsWith(".")) {
      const dir = fromPath.slice(0, fromPath.lastIndexOf("/"));
      base = normalise(dir + "/" + specifier);
    } else return null; // a package, not this tree
    for (const candidate of [
      `${base}.tsx`,
      `${base}.ts`,
      `${base}/index.tsx`,
      `${base}/index.ts`,
    ]) {
      if (byPath.has(candidate)) return candidate;
    }
    return null;
  };

  /** Local name → where it comes from, for one file's imports. */
  const importsOf = (path: string): Map<string, Imported> => {
    const cached = importsCache.get(path);
    if (cached) return cached;
    const out = new Map<string, Imported>();
    const src = byPath.get(path);
    if (src !== undefined) {
      for (const st of parsed(path, src).statements) {
        if (!ts.isImportDeclaration(st) || !st.importClause) continue;
        if (!ts.isStringLiteral(st.moduleSpecifier)) continue;
        const file = moduleFile(path, st.moduleSpecifier.text);
        const clause = st.importClause;
        if (clause.name) out.set(clause.name.text, { file, exported: null });
        const bindings = clause.namedBindings;
        if (bindings && ts.isNamedImports(bindings)) {
          for (const el of bindings.elements) {
            out.set(el.name.text, { file, exported: (el.propertyName ?? el.name).text });
          }
        }
      }
    }
    importsCache.set(path, out);
    return out;
  };

  /**
   * The nodes to walk for a component defined in `path`: its own
   * declaration, so a focusable element elsewhere in the same file is never
   * credited to it. `exported` null means the file's default export.
   */
  const declarationsOf = (path: string, exported: string | null): ts.Node[] => {
    const src = byPath.get(path);
    if (src === undefined) return [];
    const out: ts.Node[] = [];
    for (const st of parsed(path, src).statements) {
      if (ts.isFunctionDeclaration(st)) {
        const isDefault =
          (ts.getCombinedModifierFlags(st) & ts.ModifierFlags.Default) !== 0;
        if (exported === null ? isDefault : st.name?.text === exported) out.push(st);
        continue;
      }
      if (ts.isVariableStatement(st)) {
        for (const d of st.declarationList.declarations) {
          if (!ts.isIdentifier(d.name) || !d.initializer) continue;
          if (exported !== null && d.name.text === exported) out.push(d.initializer);
        }
        continue;
      }
      if (ts.isExportAssignment(st) && exported === null) out.push(st.expression);
    }
    // `export default memo(function X(){})` and `export default function(){}`
    // both land here; a default that re-exports a local name does not, and
    // its caller reads that as unresolved rather than as "nothing focusable".
    return out;
  };

  // Mutually recursive with componentFocusable, so both are declarations:
  // a const would be in its own temporal dead zone at the first reference.
  /** Is anything focusable rendered anywhere under this node? */
  function subtreeFocusable(path: string, node: ts.Node, seen: Set<string>): boolean {
    let found = false;
    const visit = (n: ts.Node): void => {
      if (found) return;
      if (ts.isJsxOpeningElement(n) || ts.isJsxSelfClosingElement(n)) {
        const tag = tagOf(n);
        const attrs = attrsOf(n);
        if (
          (isIntrinsic(tag) && intrinsicFocusable(tag, attrs)) ||
          tabbable(attrs) ||
          helperOf(n) === "clickableRowProps"
        ) {
          found = true;
          return;
        }
        if (!isIntrinsic(tag) && componentFocusable(path, tag, seen) === true) {
          found = true;
          return;
        }
      }
      ts.forEachChild(n, visit);
    };
    visit(node);
    return found;
  }

  /** Does this component render anything focusable? Unresolved stays null. */
  function componentFocusable(
    fromPath: string,
    local: string,
    seen: Set<string>,
  ): boolean | null {
    const imported = importsOf(fromPath).get(local);
    if (imported && imported.file === null) return null; // external package
    // No import entry: a component declared in the SAME file.
    const target = imported?.file ?? fromPath;
    const exported = imported ? imported.exported : local;
    const key = `${target}#${exported ?? "default"}`;
    if (seen.has(key)) return false;
    seen.add(key);
    const decls = declarationsOf(target, exported);
    if (decls.length === 0) return null;
    return decls.some((decl) => subtreeFocusable(target, decl, seen));
  }

  /**
   * The focusable descendants of a row, and the components the scan could
   * not follow. The walk descends through the row's children — a child
   * component's own markup included, because it renders inside this row.
   */
  const descendants = (
    path: string,
    element: ts.JsxElement,
  ): { focusable: string[]; unresolved: string[] } => {
    const focusable: string[] = [];
    const unresolved: string[] = [];
    const seen = new Set<string>();
    const visit = (n: ts.Node): void => {
      if (ts.isJsxOpeningElement(n) || ts.isJsxSelfClosingElement(n)) {
        const tag = tagOf(n);
        const attrs = attrsOf(n);
        if (helperOf(n) === "clickableRowProps") {
          // A row inside a row: the spread is what makes it role=button and
          // a tab stop, so no attribute of its own says so.
          focusable.push(`<${tag} clickableRowProps>`);
        } else if (isIntrinsic(tag)) {
          if (intrinsicFocusable(tag, attrs) || tabbable(attrs)) focusable.push(`<${tag}>`);
        } else if (tabbable(attrs)) {
          // The call site made it a tab stop, whatever the component is.
          focusable.push(`<${tag} tabIndex>`);
        } else {
          const verdict = componentFocusable(path, tag, seen);
          if (verdict === true) focusable.push(`<${tag}>`);
          else if (verdict === null) unresolved.push(`<${tag}>`);
        }
      }
      ts.forEachChild(n, visit);
    };
    for (const child of element.children) visit(child);
    return { focusable, unresolved };
  };

  const out: RowScan = { sites: [], nested: [], mouseOnly: [], shadowed: [], unresolved: [] };
  for (const [path, src] of byPath) {
    if (!path.endsWith(".tsx")) continue;
    if (!src.includes("RowProps(")) continue;
    const sf = parsed(path, src);
    const visit = (n: ts.Node): void => {
      if (ts.isJsxOpeningElement(n) || ts.isJsxSelfClosingElement(n)) {
        const helper = helperOf(n);
        if (helper) {
          const tag = tagOf(n);
          const base = {
            path,
            line: sf.getLineAndCharacterOfPosition(n.getStart()).line + 1,
            tag,
            helper,
          };
          out.sites.push({ ...base, detail: "" });
          const shadows = SHADOWING.filter((name) => attrsOf(n).has(name));
          if (shadows.length > 0) {
            out.shadowed.push({ ...base, detail: `own ${shadows.join(", ")}` });
          }
          const element = ts.isJsxOpeningElement(n) && ts.isJsxElement(n.parent) ? n.parent : null;
          if (element === null) {
            // A self-closing row: what it holds is rendered inside the
            // component, where this scan cannot see it. Reported rather than
            // read as "holds nothing", which would fire the mouse-only rule
            // on a row that is fine and miss a nested control that is not.
            out.unresolved.push({
              ...base,
              detail: `self-closing <${tag}>, its content is the component's own`,
            });
          } else {
            const found = descendants(path, element);
            if (!isIntrinsic(tag)) {
              // The row's own component renders around the children it is
              // given: a control it adds is inside the row too.
              const own = componentFocusable(path, tag, new Set());
              if (own === true) found.focusable.push(`<${tag}>'s own markup`);
              else if (own === null) found.unresolved.push(`<${tag}> itself`);
            }
            if (helper === "clickableRowProps" && found.focusable.length > 0) {
              out.nested.push({ ...base, detail: `holds ${found.focusable.join(" ")}` });
            }
            if (helper === "pointerRowProps" && found.focusable.length === 0) {
              out.mouseOnly.push({ ...base, detail: "holds nothing focusable" });
            }
            for (const name of new Set(found.unresolved)) {
              out.unresolved.push({ ...base, detail: name });
            }
          }
        }
      }
      ts.forEachChild(n, visit);
    };
    visit(sf);
  }
  return out;
}

/** The scan over the whole production tree — what the guard judges. */
export const scanClickableRows = (): RowScan => scanRows(files);

export const where = (h: RowHit): string =>
  `${h.path}:${h.line}  <${h.tag}> ${h.helper}${h.detail ? ` — ${h.detail}` : ""}`;
