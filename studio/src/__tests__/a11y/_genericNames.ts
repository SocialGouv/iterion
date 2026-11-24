import ts from "typescript";
import { files } from "./_scanner";

// Accessible-name-on-a-generic scan (ARIA in HTML / axe aria-prohibited-attr,
// #2767): an intrinsic element whose implicit role is generic — a bare
// <span>, <div> or <p> — must not carry aria-label / aria-labelledby. The
// rendered net only sees a surface somebody mounted (and on these a span
// comes back a violation, a div an incomplete — both fail the strict axe
// entries), while this scan is decidable over source and ends the class.
//
// The fix is never to rename the attribute: either the element earns a ROLE
// (role="img" on a glyph, role="group" / "region" / "status" on a named
// block) or the name becomes TEXT (an sr-only child, which also survives
// stylesheets being disabled). A role written as a runtime expression is
// undecidable here and counts as declared — the colocated axe entries are
// the net that bites for those.
//
// NOT a test file — imported by the guard.

export interface ProhibitedName {
  path: string;
  line: number;
  tag: string;
  /** The naming attribute found on the element. */
  attr: "aria-label" | "aria-labelledby";
  /** The role as written, when the element declares one that still
   *  prohibits naming (generic / presentation / none). */
  role: string | null;
}

/** Intrinsic elements whose implicit role is generic. */
const GENERIC = new Set(["span", "div", "p"]);

/** A role LITERAL that still prohibits naming — declaring one of these is
 *  declaring the element generic (or presentational), not giving it a role. */
const NAMELESS_ROLES = new Set(["generic", "presentation", "none"]);

const NAME_ATTRS = new Set(["aria-label", "aria-labelledby"]);

function check(
  path: string,
  sf: ts.SourceFile,
  open: ts.JsxOpeningElement | ts.JsxSelfClosingElement,
  out: ProhibitedName[],
): void {
  const tag = open.tagName.getText();
  if (!GENERIC.has(tag)) return;
  let role: string | null = null;
  let roleDeclared = false;
  let attr: ProhibitedName["attr"] | null = null;
  for (const a of open.attributes.properties) {
    if (!ts.isJsxAttribute(a)) continue;
    const name = a.name.getText();
    if (NAME_ATTRS.has(name) && a.initializer) {
      attr = name as ProhibitedName["attr"];
    } else if (name === "role" && a.initializer) {
      if (ts.isStringLiteral(a.initializer)) {
        role = a.initializer.text;
        roleDeclared = !NAMELESS_ROLES.has(role);
      } else {
        // A runtime role: undecidable from source — counts as declared.
        roleDeclared = true;
      }
    }
  }
  if (attr && !roleDeclared) {
    out.push({
      path,
      line: sf.getLineAndCharacterOfPosition(open.getStart()).line + 1,
      tag,
      attr,
      role,
    });
  }
}

/**
 * The scan over ONE source, by text: the reader this guard rests on is only
 * as good as the shapes it reads, and those are judged on hand-written
 * sources (the guard's scanner describes) rather than on whichever shapes
 * the tree happens to hold today.
 */
export function prohibitedNamesIn(path: string, src: string): ProhibitedName[] {
  const out: ProhibitedName[] = [];
  const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const visit = (n: ts.Node): void => {
    if (ts.isJsxElement(n)) check(path, sf, n.openingElement, out);
    else if (ts.isJsxSelfClosingElement(n)) check(path, sf, n, out);
    ts.forEachChild(n, visit);
  };
  visit(sf);
  return out;
}

/** The scan over the whole production tree — what the guard judges. */
export function prohibitedNames(): ProhibitedName[] {
  const out: ProhibitedName[] = [];
  for (const [path, src] of files) {
    if (!path.endsWith(".tsx")) continue;
    out.push(...prohibitedNamesIn(path, src));
  }
  return out;
}
