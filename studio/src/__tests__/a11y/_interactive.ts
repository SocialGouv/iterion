import { posix } from "node:path";

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

/**
 * The name-delegating form primitives: each renders a bare control and
 * leaves the accessible name to its CALL SITE. `INTERACTIVE` above reads
 * intrinsic tags only, so every one of these elements is invisible to that
 * scan — and source-discipline.test.ts bans a raw `<select>` or a raw
 * `<input type="checkbox|radio">` outside them, which is what routes the
 * whole select/checkbox/radio naming class through tags the intrinsic scan
 * cannot see.
 */
const PRIMITIVES = new Set(["Input", "Select", "Textarea", "Checkbox", "Radio", "TagInput"]);

/**
 * The tags an enclosing `<label>` can name. A link is not one of them.
 *
 * The primitives are in it for BOTH scans: which element consumes the
 * label's single offer is a fact about the rendered DOM, not about the tags
 * a caller happens to be looking for, and two scans disagreeing about who
 * spent it would each credit a control the other says is bare.
 */
const LABELABLE = new Set(["button", "input", "textarea", "select", ...PRIMITIVES]);

/**
 * The tags whose CONTENTS are their accessible name — `role=button` and
 * `role=link` take their name from content, and nothing else here does.
 *
 * A `<select>`'s children are its `<option>`s and a `<textarea>`'s child is
 * its VALUE: neither contributes to the control's name, so crediting a
 * paired element for having children names the whole select/checkbox/radio
 * class out of this scan. The blast radius was one site —
 * source-discipline.test.ts bans raw `<select>` outside ui/Select.tsx — and
 * that site is exactly why ui/Select.tsx was the one form primitive absent
 * from naming.test.ts's waiver list.
 */
const NAMED_BY_CONTENT = new Set(["button", "a"]);

/**
 * A label's offer of its name, consumed by the first labelable descendant.
 * `text` is what the label announces when the source writes it statically;
 * `null` is a runtime child or an aria name this reader cannot read, which
 * names the control all the same.
 */
interface LabelOffer {
  free: boolean;
  text: string | null;
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

/**
 * The literal text an attribute was written with, in EITHER spelling —
 * `aria-label="x"` and `aria-label={"x"}` (or `{`x`}`) render the identical
 * DOM, so the same element written paired must get the same verdict. `null`
 * is "this reader cannot decide", which is what a runtime value is.
 */
const staticText = (init: ts.Expression | undefined): string | null => {
  if (!init) return null;
  if (ts.isStringLiteral(init)) return init.text;
  if (ts.isJsxExpression(init) && init.expression) {
    const e = init.expression;
    if (ts.isStringLiteral(e) || ts.isNoSubstitutionTemplateLiteral(e)) return e.text;
  }
  return null;
};

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
 * The key a `htmlFor` / `id` value is matched under, in the ONE spelling both
 * sides must agree on. A literal is keyed by its TEXT and an undecidable
 * expression by its source, in separate namespaces: `htmlFor={email}` reads a
 * variable, `id="email"` writes the string, and the two are the same key only
 * by accident of spelling — pairing them would clear a control nothing names.
 *
 * `<label htmlFor="email">` + `<Input id="email">` is the plain-HTML idiom and
 * the majority spelling in this tree: a reader that took `{braces}` alone
 * would call 79 correctly-labelled call sites unnamed.
 */
const idKey = (init: ts.Expression | ts.StringLiteral | undefined): string | null => {
  if (!init) return null;
  const lit = staticText(init);
  if (lit !== null) return `lit\u0000${lit}`;
  if (ts.isJsxExpression(init) && init.expression) return `expr\u0000${init.expression.getText()}`;
  return null;
};

/**
 * Does a `<label htmlFor={x}>` in this file point at this element's `id={x}`?
 * Read for a PAIRED element only: the match is file-scoped, so it would also
 * pair a label in one branch with a control in another (see the self-closing
 * branch, where that shape is real).
 */
function labelledById(
  attrs: Map<string, ts.Expression | ts.StringLiteral | undefined>,
  labelledIds: ReadonlyMap<string, string | null>,
): boolean {
  const key = idKey(attrs.get("id"));
  return key !== null && labelledIds.has(key);
}

/** An id an enclosing render prop minted, with the subtree it is in scope for. */
interface MintedId {
  name: string;
  start: number;
  end: number;
}

/**
 * Does this element's `id={x}` come from a render prop that minted `x` and
 * ENCLOSES it? The binding is read with its range, not as a file-scoped
 * name: any `id={x}` in a file whose render prop happens to destructure an
 * `x` would otherwise be cleared, for any property name, which is a hole the
 * guard exists to close.
 */
function mintedByRenderProp(
  attrs: Map<string, ts.Expression | ts.StringLiteral | undefined>,
  minted: readonly MintedId[],
  el: ts.Node,
): boolean {
  const init = attrs.get("id");
  if (!init || !ts.isJsxExpression(init) || !init.expression) return false;
  const name = init.expression.getText();
  const at = el.getStart();
  return minted.some((m) => m.name === name && at >= m.start && at < m.end);
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

/**
 * The offer a `<label>` makes, with the text it announces when the source
 * writes it: a static `aria-label` on the label, else its own static text.
 * `null` text is a name this reader cannot read — a runtime child, a
 * runtime aria-label, an `aria-labelledby` — and names the control all the
 * same. No offer at all from a label that names nothing.
 */
function offerOf(label: ts.JsxElement): LabelOffer | null {
  if (!names(label)) return null;
  const attrs = attrsOf(label.openingElement);
  if (namesStatically(attrs.get("aria-label"))) {
    return { free: true, text: staticText(attrs.get("aria-label")) };
  }
  if (attrs.has("aria-labelledby")) return { free: true, text: null };
  return { free: true, text: hasExpressionChild(label) ? null : ownText(label) };
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
 *  element is therefore not flagged as unnamed.
 *
 *  Read as a NAME only for a tag in {@link NAMED_BY_CONTENT} (or for a
 *  `<label>`, whose children are the name it offers): a `<select>`'s runtime
 *  `<option>`s and a `<textarea>`'s runtime value are children that name
 *  nothing. */
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

/**
 * What a scan judges, and what names a site it judges. Both scans share ONE
 * walk: the enclosing-label offer and the file-scoped `htmlFor`/`id` pairs
 * are facts about the source, and two walks would be two readings of them.
 */
interface Judged {
  /** The tags reported when nothing names them. */
  tags: Set<string>;
  /** The tags whose CONTENTS are their accessible name. */
  namedByContent: Set<string>;
  /**
   * Attributes that name the control here, beyond `aria-label`,
   * `aria-labelledby` and `title` — `label` is Checkbox's and Radio's own
   * prop, which the primitive turns into its `<label>` wrapper.
   */
  nameProps: readonly string[];
  /** Read the file-scoped `htmlFor`/`id` pair on a SELF-CLOSING element too? */
  pairOnSelfClosing: boolean;
  /**
   * Take an `id` minted by an ENCLOSING render prop (`<FieldRow>{({ inputId
   * }) => …}</FieldRow>`) for named: the component that minted it is the one
   * rendering the `<label htmlFor>`, in ANOTHER file. Scoped to the render
   * prop's own subtree — a file-scoped binding name would clear any `id={x}`
   * in a file where some render prop destructures an `x`.
   *
   * It clears no site in the tree today: `FieldRow`'s render-prop form is
   * used only inside FormField.tsx, where the `<label htmlFor>` sits in the
   * same file and the pair above already clears those sites. `FieldRow` is
   * exported, so the rule is what keeps its first consumer elsewhere off the
   * ledger instead of on it with no truthful reason.
   */
  idFromRenderProp: boolean;
  /**
   * Report a site whose name could arrive through a `{...spread}` instead of
   * guessing. Reading it as named is a hole the guard exists to close;
   * reading it as unnamed would be a lie about a correct call site — so it
   * is reported as what it is, a site this reader REFUSES.
   */
  reportSpread: boolean;
}

const INTRINSIC: Judged = {
  tags: INTERACTIVE,
  namedByContent: NAMED_BY_CONTENT,
  nameProps: [],
  // Deliberately NOT on a self-closing element: the match is FILE-scoped,
  // and a primitive with a labelled and an unlabelled branch (Checkbox,
  // Radio) would have its unlabelled control named by the other branch's
  // htmlFor. A false positive here costs a waiver naming the consumer's
  // test; a false negative costs the class the guard exists for.
  pairOnSelfClosing: false,
  idFromRenderProp: false,
  reportSpread: false,
};

const PRIMITIVE: Judged = {
  tags: PRIMITIVES,
  // A `<Select>`'s children are its `<option>`s: never a name.
  namedByContent: new Set<string>(),
  nameProps: ["label"],
  // A call site is the ONE branch that renders, so the file-scoped pair says
  // what it says — the ambiguity the intrinsic scan refuses belongs to a
  // primitive's own two branches, not to its callers.
  pairOnSelfClosing: true,
  idFromRenderProp: true,
  reportSpread: true,
};

/** Why a site could not be cleared. */
export type UnnamedWhy =
  /** No naming mechanism at all. */
  | "unnamed"
  /** A `{...spread}` could carry one, and this reader will not guess. */
  | "unreadable";

export interface UnnamedPrimitiveSite extends UnnamedInteractive {
  why: UnnamedWhy;
}

/** The intrinsic scan over the whole production tree. */
export function unnamedInteractives(): UnnamedInteractive[] {
  return scanTree(INTRINSIC).map(({ path, line, tag, classes }) => ({
    path,
    line,
    tag,
    classes,
  }));
}

export interface PrimitiveScan {
  /** How many call sites the scan JUDGED — a guard's non-vacuity claim. */
  judged: number;
  /** The ones nothing readable in the source names. */
  unnamed: UnnamedPrimitiveSite[];
}

/**
 * The call sites of the name-delegating primitives, over the whole
 * production tree: the half of RGAA 11.1 `unnamedInteractives` structurally
 * cannot see, because `<Input>` / `<Select>` / `<Checkbox>` are components
 * and it reads intrinsic tags only.
 *
 * The judged COUNT travels with the findings rather than being recomputed by
 * the caller: a reader that re-greps the tree for `<Input` is judging a
 * different population than the one these findings came from.
 */
export function primitiveSites(): PrimitiveScan {
  const tally = { judged: 0 };
  const unnamed: UnnamedPrimitiveSite[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    unnamed.push(...scanSource(path, src, PRIMITIVE, tally));
  }
  return { judged: tally.judged, unnamed };
}

function scanTree(judged: Judged): UnnamedPrimitiveSite[] {
  const out: UnnamedPrimitiveSite[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    out.push(...scanSource(path, src, judged));
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
  return scanSource(path, src, INTRINSIC).map(({ path: p, line, tag, classes }) => ({
    path: p,
    line,
    tag,
    classes,
  }));
}

/** The primitive scan over ONE source — same reason, same discipline. */
export function unnamedPrimitiveSitesIn(path: string, src: string): UnnamedPrimitiveSite[] {
  return scanSource(path, src, PRIMITIVE);
}

/**
 * A wrapper that renders a naming `<label>` AROUND its `{children}`:
 * `function Field({ label, children }) { return <label><span>{label}</span>
 * {children}</label>; }`. A control written under `<Field label="Name">` is
 * named by the DOM exactly as one written under `<label>`, and a reader that
 * saw only the literal tag would put every such call site on the ledger as
 * bare.
 */
interface LabelWrapper {
  /** The label's own static text. */
  text: string;
  /**
   * The call-site props the label's text is composed of (`{label}` reads the
   * `label` prop), or `null` when that text is not plain props — the control
   * is named either way, only the NAME is then unreadable here.
   */
  props: readonly string[] | null;
  /** Does the label announce something of its own, before the call site's children? */
  names: boolean;
}

/**
 * The label wrappers a source declares, by component name. A component is
 * one when its body renders a `<label>` whose children include
 * `{children}` (or `{props.children}`) — the wrapper's `<label>` is read as
 * written, so one rendered on a condition is credited whatever the
 * condition. Read from `function X(…)` and `const X = (…) => …`; a
 * component wrapped in `memo`/`forwardRef` is not read, and its call sites
 * stay on the ledger with their own reason.
 */
function labelWrappersOf(sf: ts.SourceFile): Map<string, LabelWrapper> {
  const wrappers = new Map<string, LabelWrapper>();
  const consider = (
    name: string,
    fn: ts.ArrowFunction | ts.FunctionExpression | ts.FunctionDeclaration,
  ): void => {
    if (!/^[A-Z]/.test(name) || !fn.body) return;
    // The local binding of each prop: `{ label: text }` reads `label` as `text`.
    const bound = new Map<string, string>();
    let propsParam: string | null = null;
    for (const param of fn.parameters) {
      if (ts.isObjectBindingPattern(param.name)) {
        for (const element of param.name.elements) {
          if (!ts.isIdentifier(element.name)) continue;
          const prop =
            element.propertyName && ts.isIdentifier(element.propertyName)
              ? element.propertyName.text
              : element.name.text;
          bound.set(element.name.text, prop);
        }
      } else if (ts.isIdentifier(param.name) && propsParam === null) {
        propsParam = param.name.text;
      }
    }
    const propOf = (e: ts.Expression): string | null => {
      if (ts.isIdentifier(e)) return bound.get(e.text) ?? null;
      if (
        ts.isPropertyAccessExpression(e) &&
        ts.isIdentifier(e.expression) &&
        e.expression.text === propsParam
      ) {
        return e.name.text;
      }
      return null;
    };
    const find = (n: ts.Node): void => {
      if (wrappers.has(name)) return;
      if (ts.isJsxElement(n) && tagOf(n.openingElement) === "label") {
        let wrapsChildren = false;
        let props: string[] | null = [];
        const walk = (c: ts.Node): void => {
          if (ts.isJsxAttribute(c)) return;
          if (ts.isJsxExpression(c) && c.expression) {
            const prop = propOf(c.expression);
            if (prop === "children") wrapsChildren = true;
            else if (prop !== null) props?.push(prop);
            else props = null;
            return;
          }
          ts.forEachChild(c, walk);
        };
        for (const child of n.children) walk(child);
        if (wrapsChildren) {
          const text = ownText(n);
          wrappers.set(name, {
            text,
            props,
            names: text.length > 0 || props === null || props.length > 0,
          });
          return;
        }
      }
      ts.forEachChild(n, find);
    };
    find(fn.body);
  };
  const top = (n: ts.Node): void => {
    if (ts.isFunctionDeclaration(n) && n.name) consider(n.name.text, n);
    if (
      ts.isVariableDeclaration(n) &&
      ts.isIdentifier(n.name) &&
      n.initializer &&
      (ts.isArrowFunction(n.initializer) || ts.isFunctionExpression(n.initializer))
    ) {
      consider(n.name.text, n.initializer);
    }
    ts.forEachChild(n, top);
  };
  top(sf);
  return wrappers;
}

/**
 * The wrappers of a TREE source, parsed once: the tree is immutable for the
 * run, and a source handed in by text (the scanner witnesses) is never one.
 */
const TREE_WRAPPERS = new Map<string, Map<string, LabelWrapper>>();

function treeWrappersOf(path: string, src: string): Map<string, LabelWrapper> {
  const cached = TREE_WRAPPERS.get(path);
  if (cached) return cached;
  const wrappers = labelWrappersOf(
    ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX),
  );
  TREE_WRAPPERS.set(path, wrappers);
  return wrappers;
}

/**
 * The label wrappers in scope of a source: its own, and the ones it IMPORTS
 * from another source of the tree (`import { Field } from "./orgFields"`,
 * under the local name the import binds). The wrapper's definition is one
 * resolved file away, which is a fact this reader can read — unlike a
 * `htmlFor` handed over as a prop, whose other side is any caller's.
 */
function labelWrappersInScope(path: string, sf: ts.SourceFile): Map<string, LabelWrapper> {
  const inScope = new Map<string, LabelWrapper>(labelWrappersOf(sf));
  for (const stmt of sf.statements) {
    if (!ts.isImportDeclaration(stmt) || !ts.isStringLiteral(stmt.moduleSpecifier)) continue;
    const named = stmt.importClause?.namedBindings;
    if (!named || !ts.isNamedImports(named)) continue;
    const target = resolveImport(path, stmt.moduleSpecifier.text);
    if (target === null) continue;
    const theirs = treeWrappersOf(target[0], target[1]);
    for (const spec of named.elements) {
      const exported = spec.propertyName ? spec.propertyName.text : spec.name.text;
      const wrapper = theirs.get(exported);
      if (wrapper) inScope.set(spec.name.text, wrapper);
    }
  }
  return inScope;
}

/** A relative or `@/` import, resolved to a source of the tree. */
function resolveImport(from: string, specifier: string): [string, string] | null {
  let base: string;
  if (specifier.startsWith("@/")) base = `/src/${specifier.slice(2)}`;
  else if (specifier.startsWith(".")) base = posix.resolve(posix.dirname(from), specifier);
  else return null;
  for (const candidate of [`${base}.tsx`, `${base}.ts`, `${base}/index.tsx`, `${base}/index.ts`]) {
    const found = files.find(([path]) => path === candidate);
    if (found) return found;
  }
  return null;
}

/**
 * What a source says about who names what — read ONCE, and judged by both
 * naming scans and the row-action scan. The enclosing-label offers, the
 * file-scoped `htmlFor`/`id` pairs and the label wrappers are facts about
 * the source; two walks would be two readings of them, and two readings
 * disagreeing about who spent an offer would each credit a control the
 * other says is bare.
 */
interface SourceFacts {
  sf: ts.SourceFile;
  /**
   * The htmlFor↔id associations whose TWO SIDES carry the same value, keyed
   * by {@link idKey}: a literal (`htmlFor="email"` / `id="email"`, the
   * plain-HTML idiom) or the same expression source (the FieldRow useId
   * pattern: `htmlFor={inputId}` / `id={inputId}`), each with the label's
   * static text (`null` = runtime, or two labels disagreeing). A cross-file
   * or runtime-only association stays undecidable here.
   */
  labelledIds: Map<string, string | null>;
  /**
   * The ids an enclosing component MINTED and handed down through a render
   * prop. `<FieldRow>{({ inputId }) => <Input id={inputId} />}</FieldRow>`:
   * the `<label htmlFor={inputId}>` lives in FormField.tsx, so no
   * file-scoped pair can see it, and the binding is the only trace left.
   * Read from a function whose DIRECT parent is a JsxExpression — a child or
   * an attribute — so `{rows.map(({ id }) => <Input id={id} />)}`, whose
   * arrow is a call argument, is not mistaken for one. Each binding carries
   * the arrow's own range: the id is in scope THERE and nowhere else in the
   * file.
   */
  renderPropIds: MintedId[];
  /** Element start → the label offer that element consumed. */
  claimed: Map<number, LabelOffer>;
}

function readSource(path: string, src: string): SourceFacts {
  const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const labelledIds = new Map<string, string | null>();
  const renderPropIds: MintedId[] = [];
  const collect = (n: ts.Node): void => {
    if (ts.isJsxAttribute(n) && n.name.getText() === "htmlFor") {
      const key = idKey(n.initializer ?? undefined);
      if (key !== null) {
        // The label is the attribute's owner: its text is what the control
        // it points at announces. A self-closing owner has none to read.
        const owner = n.parent.parent;
        const text =
          ts.isJsxOpeningElement(owner) && !hasExpressionChild(owner.parent)
            ? ownText(owner.parent)
            : null;
        labelledIds.set(key, labelledIds.has(key) && labelledIds.get(key) !== text ? null : text);
      }
    }
    if ((ts.isArrowFunction(n) || ts.isFunctionExpression(n)) && ts.isJsxExpression(n.parent)) {
      for (const param of n.parameters) {
        if (!ts.isObjectBindingPattern(param.name)) continue;
        for (const element of param.name.elements) {
          if (ts.isIdentifier(element.name)) {
            renderPropIds.push({
              name: element.name.getText(),
              start: n.getStart(),
              end: n.getEnd(),
            });
          }
        }
      }
    }
    ts.forEachChild(n, collect);
  };
  collect(sf);
  const wrappers = labelWrappersInScope(path, sf);

  /**
   * The enclosing `<label>` a control may still claim. An HTML label names
   * its FIRST labelable descendant and no other, so the offer is consumed:
   * the second control under one label is as unnamed as one standing alone
   * (a from/to pair under a single "Date range" label is the live shape).
   * Mutable and shared across the label's subtree, which is what makes
   * "first in document order" readable from a recursive walk.
   */
  const claimed = new Map<number, LabelOffer>();
  const claim = (label: LabelOffer | null, tag: string, at: number): void => {
    if (!label || !label.free || !LABELABLE.has(tag)) return;
    label.free = false;
    claimed.set(at, label);
  };

  /**
   * The offer a wrapper's call site makes to the children it wraps: the
   * wrapper's label text plus the call site's own, when both are static.
   */
  const wrapperOffer = (el: ts.JsxElement, wrapper: LabelWrapper, attrs: Attrs): LabelOffer | null => {
    const ownNames = ownText(el).length > 0 || hasExpressionChild(el);
    if (!wrapper.names && !ownNames) return null;
    if (wrapper.props === null || hasExpressionChild(el)) return { free: true, text: null };
    const parts = [wrapper.text];
    for (const prop of wrapper.props) {
      const init = attrs.get(prop);
      if (init === undefined) continue;
      const lit = staticText(init);
      if (lit === null) return { free: true, text: null };
      parts.push(lit);
    }
    parts.push(ownText(el));
    return { free: true, text: parts.filter(Boolean).join(" ") };
  };

  const visit = (n: ts.Node, label: LabelOffer | null): void => {
    if (ts.isJsxElement(n)) {
      const tag = tagOf(n.openingElement);
      let inner = label;
      if (tag) {
        const attrs = attrsOf(n.openingElement);
        // input[type=hidden] is not a labelable element, so it neither needs
        // a name nor spends its label's offer — in either spelling, since
        // the two branches owe each other the same verdict.
        if (!(tag === "input" && staticText(attrs.get("type")) === "hidden")) {
          claim(label, tag, n.getStart());
        }
        // forEachChild only ever hands back a CHILD, so the children of a
        // <label> — or of a wrapper that renders one around them — are
        // exactly the nodes that inherit its offer. A label with no text and
        // no runtime child names nothing, and a control it wraps is as
        // unnamed as one standing alone.
        const wrapper = wrappers.get(tag);
        const offer = tag === "label" ? offerOf(n) : wrapper ? wrapperOffer(n, wrapper, attrs) : null;
        if (offer) inner = offer;
      }
      ts.forEachChild(n, (c) => visit(c, inner));
      return;
    }
    if (ts.isJsxSelfClosingElement(n)) {
      const tag = tagOf(n);
      if (tag) {
        const attrs = attrsOf(n);
        // Claimed BEFORE anything else can return or short-circuit: a
        // control that is named another way still spends the offer, or the
        // next bare sibling would claim a label HTML never gave it. The
        // paired branch above does the same, and the two must agree.
        if (!(tag === "input" && staticText(attrs.get("type")) === "hidden")) {
          claim(label, tag, n.getStart());
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
        const fork: LabelOffer = { free: label.free, text: label.text };
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
  return { sf, labelledIds, renderPropIds, claimed };
}

function scanSource(
  path: string,
  src: string,
  judged: Judged,
  tally?: { judged: number },
): UnnamedPrimitiveSite[] {
  const facts = readSource(path, src);
  const out: UnnamedPrimitiveSite[] = [];
  const judge = (n: ts.Node): void => {
    if (ts.isJsxElement(n) || ts.isJsxSelfClosingElement(n)) {
      const open = ts.isJsxElement(n) ? n.openingElement : n;
      const tag = tagOf(open);
      if (tag !== null && judged.tags.has(tag)) {
        const attrs = attrsOf(open);
        // input[type=hidden] is not a labelable element: it needs no name.
        if (!(tag === "input" && staticText(attrs.get("type")) === "hidden")) {
          if (tally) tally.judged += 1;
          const byContent =
            ts.isJsxElement(n) &&
            judged.namedByContent.has(tag) &&
            (ownText(n).length > 0 || hasExpressionChild(n));
          const named =
            // HiddenFileInput's shape: a picker a button drives, declared out
            // of the tree and out of the tab order.
            outOfTree(attrs) ||
            byContent ||
            namedByAttribute(attrs, judged) ||
            ((ts.isJsxElement(n) || judged.pairOnSelfClosing) &&
              labelledById(attrs, facts.labelledIds)) ||
            (judged.idFromRenderProp && mintedByRenderProp(attrs, facts.renderPropIds, n)) ||
            facts.claimed.has(n.getStart());
          if (!named) out.push(site(path, facts.sf, n, tag, attrs, judged));
        }
      }
    }
    ts.forEachChild(n, judge);
  };
  judge(facts.sf);
  return out;
}

/** The naming attributes any judged tag answers to: the aria pair, title, and the scan's own props. */
function namedByAttribute(attrs: Attrs, judged: Judged): boolean {
  return (
    namesStatically(attrs.get("aria-label")) ||
    namesStatically(attrs.get("aria-labelledby")) ||
    namesStatically(attrs.get("title")) ||
    judged.nameProps.some((prop) => namesStatically(attrs.get(prop)))
  );
}

function site(
  path: string,
  sf: ts.SourceFile,
  el: ts.JsxElement | ts.JsxSelfClosingElement,
  tag: string,
  attrs: Attrs,
  judged: Judged,
): UnnamedPrimitiveSite {
  const open = ts.isJsxElement(el) ? el.openingElement : el;
  const spread = open.attributes.properties.some(ts.isJsxSpreadAttribute);
  return {
    path,
    line: sf.getLineAndCharacterOfPosition(el.getStart()).line + 1,
    tag,
    classes: classesOf(attrs),
    why: spread && judged.reportSpread ? "unreadable" : "unnamed",
  };
}

function tagOf(el: ts.JsxOpeningElement | ts.JsxSelfClosingElement): string | null {
  const name = el.tagName;
  return ts.isIdentifier(name) || ts.isJsxNamespacedName(name)
    ? (name as { getText(): string }).getText()
    : null;
}

// ── #2591 leg 1: the repeated row action ────────────────────────────────
//
// `expectUniqueAccessibleNames` bites per SURFACE, on a rendered tree, and
// axe has no rule for two controls answering to one accessible name at any
// level. This scan is the class-level half: a control inside a row map whose
// name is written STATICALLY announces the same thing on every row, which is
// the shape the surfaces #2591 fixed had ("Rotate", "Delete", "Replay" per
// row). Whether two of them are on screen at once depends on the data — a
// map can render the control for one row only — so a hit names a surface to
// AUDIT, and naming.test.ts's remainder is what makes the population a
// ratchet rather than a sentence.
//
// The name is read in the accessible-name algorithm's own precedence —
// `aria-labelledby`, `aria-label` (and the props a component turns into
// one), the consumer's `<label>` (enclosing, through a wrapper, or by
// `htmlFor`/`id` in this file), the CONTENTS of a content-named control,
// then `title` — so a `<button title="Open">{r.name}</button>` is the row's
// own name and a `<Button title={r.hint}>Delete</Button>` is "Delete" on
// every row, whatever the hint says.
//
// Four populations it does NOT judge, named so a reader knows the boundary:
// a control named at runtime (the fix for this class); a row whose own
// CONTENT is its name — `clickableRowProps` puts `role="button"` on a
// `<Tr>`, so RetriesTable, MemoryTab, LogLinesView and the other
// content-named rows carry one control per row that this reader does not
// see; a control whose `<label htmlFor>` lives in ANOTHER file, where no
// file-scoped pair can read the text; and a row map whose callback is
// passed BY REFERENCE (`rows.map(renderRow)`), where the row's JSX lives in
// another function and no walk from the call reaches it. No `.map(fnRef)`
// in the tree renders JSX today (MarkdownText and SequentialReviews map over
// data), so the limit is a declared blind spot rather than a current miss.

/**
 * The form-field wrappers of `components/Panels/forms/FormField.tsx`: each
 * renders ONE control and takes a REQUIRED `label`, which `FieldRow` turns
 * into the control's `<label htmlFor>`. "Name" on every row of a vars editor
 * is therefore one announcement repeated per row, exactly like "Delete".
 *
 * `FieldRow` itself is in the list: it is the one that mints the id and
 * writes the `<label htmlFor>`, and it is exported, so the rule is what
 * keeps its first consumer outside FormField.tsx off this scan's blind side.
 * It names no site in the tree today — its seven uses are FormField.tsx's
 * own, none of them in a row map.
 */
const FIELD_WRAPPERS = [
  "FieldRow",
  "TextField",
  "CommittedTextField",
  "NumberField",
  "SelectField",
  "SelectFieldWithCreate",
  "CheckboxField",
  "TagListField",
  "PromptPickerField",
  "MultiSelectField",
] as const;

/**
 * The row controls this scan reads, each with the props that name it beyond
 * `aria-label` and `title`: `IconButton.label` is its own documented
 * accessible label, and `CopyButton` takes `label` plus `describes`, the
 * qualifier that already solves this class there.
 *
 * The name-delegating {@link PRIMITIVES} are in it, derived from the one set
 * rather than re-listed: a `<Checkbox aria-label="Enable trigger">` or an
 * `<Input aria-label="Variable name">` inside a row map announces the same
 * thing on every row just as a `<Button>Delete</Button>` does, and a tag the
 * primitive naming guard judges must not be invisible here — a scope
 * narrower than the class would make this net blind to the one regression it
 * exists for.
 */
const ROW_CONTROL_NAME_PROPS: Record<string, readonly string[]> = {
  button: [],
  a: [],
  Button: [],
  IconButton: ["label"],
  CopyButton: ["label", "describes"],
  ...Object.fromEntries(
    [...PRIMITIVES, ...FIELD_WRAPPERS].map((tag) => [tag, ["label"] as readonly string[]]),
  ),
};

/** The row controls whose CONTENTS are their accessible name. */
const ROW_CONTROL_BY_CONTENT = new Set(["button", "a", "Button"]);

/** The name a row control announces when its own naming prop is absent. */
const ROW_CONTROL_DEFAULT_NAME: Record<string, string> = { CopyButton: "copy" };

/**
 * The name a row control announces, when the source writes every spelling of
 * it: a literal, or a conditional between literals — a per-row toggle
 * (`aria-label={row.on ? "Disable" : "Enable"}`) announces one of two fixed
 * names, so two rows in the same state still answer to one name. `null` is a
 * name this reader cannot decide, which a row's own data is.
 *
 * `staticText` stays the shared reader for the naming guards; this one is the
 * row scan's own, so a widening here cannot move their ledgers.
 */
function staticRowName(init: ts.Expression | ts.StringLiteral | undefined): string | null {
  const literal = (e: ts.Expression): string | null => {
    if (ts.isParenthesizedExpression(e)) return literal(e.expression);
    if (ts.isStringLiteral(e) || ts.isNoSubstitutionTemplateLiteral(e)) return e.text;
    if (ts.isConditionalExpression(e)) {
      const whenTrue = literal(e.whenTrue);
      const whenFalse = literal(e.whenFalse);
      return whenTrue === null || whenFalse === null ? null : `${whenTrue} / ${whenFalse}`;
    }
    return null;
  };
  if (!init) return null;
  if (ts.isJsxExpression(init)) return init.expression ? literal(init.expression) : null;
  return literal(init);
}

export interface RowActionSite {
  /** 1-based line of the control's opening tag. */
  line: number;
  tag: string;
  /**
   * The name every row announces, as the source writes it — `A / B` when a
   * per-row toggle picks between two fixed names.
   */
  name: string;
  /** A `{...spread}` could carry a name — this reader refuses to guess. */
  unreadable: boolean;
}

export interface RowActionSurface {
  path: string;
  sites: RowActionSite[];
}

export interface RowActionScan {
  /** How many row controls the scan JUDGED — a guard's non-vacuity claim. */
  judged: number;
  surfaces: RowActionSurface[];
}

/**
 * The statically-named controls inside a row map, over the whole production
 * tree. The judged COUNT travels with the findings: a reader that re-greps
 * for `<Button` is judging a different population than these came from.
 */
export function repeatedRowActions(): RowActionScan {
  const tally = { judged: 0 };
  const surfaces: RowActionSurface[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    const sites = rowActionSites(path, src, tally);
    if (sites.length > 0) surfaces.push({ path, sites });
  }
  surfaces.sort((a, b) => a.path.localeCompare(b.path));
  return { judged: tally.judged, surfaces };
}

/** The same scan over ONE source — the reader is judged on written shapes. */
export function repeatedRowActionsIn(path: string, src: string): RowActionSite[] {
  return rowActionSites(path, src);
}

/** `rows.map(row => …)`: the callback's body is ONE row, rendered per item. */
function isRowMap(n: ts.Node): n is ts.CallExpression {
  if (!ts.isCallExpression(n) || !ts.isPropertyAccessExpression(n.expression)) return false;
  const method = n.expression.name.getText();
  if (method !== "map" && method !== "flatMap") return false;
  const cb = n.arguments[0];
  return cb !== undefined && (ts.isArrowFunction(cb) || ts.isFunctionExpression(cb));
}

function rowActionSites(
  path: string,
  src: string,
  tally?: { judged: number },
): RowActionSite[] {
  const facts = readSource(path, src);
  // Keyed by the element's START POSITION, so two controls written on one
  // line are two sites. The LINE is what the report shows and what the
  // ledger's count ratchets on; keyed by it, a second control written beside
  // the first would REPLACE it and leave the count at one, which is the
  // ratchet's own defeat.
  const byElement = new Map<number, RowActionSite>();
  const visit = (n: ts.Node, inRowMap: boolean): void => {
    if (isRowMap(n)) {
      // The RECEIVER is not inside the row — `rows` can hold a map itself.
      visit(n.expression, inRowMap);
      for (const arg of n.arguments) visit(arg, true);
      return;
    }
    if (ts.isJsxElement(n) || ts.isJsxSelfClosingElement(n)) {
      const open = ts.isJsxElement(n) ? n.openingElement : n;
      const tag = tagOf(open);
      if (inRowMap && tag !== null && tag in ROW_CONTROL_NAME_PROPS) {
        if (tally) tally.judged += 1;
        const site = rowActionSite(facts, n, open, tag);
        if (site) byElement.set(n.getStart(), site);
      }
    }
    ts.forEachChild(n, (c) => visit(c, inRowMap));
  };
  visit(facts.sf, false);
  return [...byElement.entries()]
    .sort(([a], [b]) => a - b)
    .map(([, site]) => site);
}

/**
 * What the CONSUMER's label announces for this control — the mechanisms
 * the primitive naming guard reads, read off the same facts: the enclosing
 * `<label>` (or wrapper) whose offer this element consumed, else the
 * `<label htmlFor>` in this file pointing at its `id`. "GitHub org" on
 * every grant row is one announcement repeated per row exactly as "Remove"
 * is. `undefined` is no such label; `null` is one whose text this reader
 * cannot decide.
 */
function consumerName(facts: SourceFacts, el: ts.Node, attrs: Attrs): string | null | undefined {
  const offer = facts.claimed.get(el.getStart());
  if (offer) return offer.text;
  const key = idKey(attrs.get("id"));
  if (key !== null && facts.labelledIds.has(key)) return facts.labelledIds.get(key) ?? null;
  return undefined;
}

/** The site, when the name this control announces is the same on every row. */
function rowActionSite(
  facts: SourceFacts,
  el: ts.JsxElement | ts.JsxSelfClosingElement,
  open: ts.JsxOpeningElement | ts.JsxSelfClosingElement,
  tag: string,
): RowActionSite | null {
  const attrs = attrsOf(open);
  // Out of the accessibility tree AND out of the tab order: it announces
  // nothing, so it cannot announce the same thing twice.
  if (outOfTree(attrs)) return null;
  // `aria-labelledby` takes the name from another element, whose text this
  // reader does not follow.
  if (attrs.has("aria-labelledby")) return null;
  // The props that COMPOSE the control's `aria-label`, in the order the
  // component concatenates them — `CopyButton` announces `label` then
  // `describes`, which is why one undecidable part makes the whole name
  // undecidable rather than leaving the other part standing as the name.
  const labelProps = ["aria-label", ...(ROW_CONTROL_NAME_PROPS[tag] ?? [])].filter((prop) =>
    attrs.has(prop),
  );
  let name: string | null = null;
  if (labelProps.length > 0) {
    const parts: string[] = [];
    for (const prop of labelProps) {
      const lit = staticRowName(attrs.get(prop));
      // A runtime value is the FIX for this class: the row qualifies its own
      // control, and which row it is cannot be read here.
      if (lit === null) return null;
      if (lit.trim()) parts.push(lit);
    }
    if (parts.length > 0) name = parts.join(" ");
  }
  // The consumer's `<label>`, next in the algorithm: a runtime text there is
  // the row's own name.
  if (name === null) {
    const consumer = consumerName(facts, el, attrs);
    if (consumer === null) return null;
    if (consumer !== undefined && consumer.trim()) name = consumer;
  }
  // The CONTENTS come before `title`: a content-named control announces its
  // text, and a runtime child is the row's own data — `<Button
  // title="Open">{row.name}</Button>` announces a different name on every
  // row, whatever the title holds.
  if (name === null && ROW_CONTROL_BY_CONTENT.has(tag) && ts.isJsxElement(el)) {
    if (hasExpressionChild(el)) return null;
    const text = ownText(el);
    if (text) name = text;
  }
  // `title` is the accessible name only when NOTHING above gave one: it
  // sits at the bottom of the algorithm, so a runtime title beside a static
  // aria-label or static contents cannot change what is announced, and
  // treating it as undecidable would drop a statically-named control from
  // the scan.
  if (name === null && attrs.has("title")) {
    const lit = staticRowName(attrs.get("title"));
    if (lit === null) return null;
    if (lit.trim()) name = lit;
  }
  const announced = name ?? ROW_CONTROL_DEFAULT_NAME[tag] ?? null;
  // A control this reader finds NO name for announces nothing, which is the
  // unnamed-control class — `unnamedInteractives` and `primitiveSites` above
  // are its guards. Reporting it here as a repeated name would say two rows
  // share an announcement neither of them makes.
  if (announced === null || !announced.trim()) return null;
  return {
    line: facts.sf.getLineAndCharacterOfPosition(el.getStart()).line + 1,
    tag,
    name: announced,
    unreadable: open.attributes.properties.some(ts.isJsxSpreadAttribute),
  };
}
