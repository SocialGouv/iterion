// The bound of the whole-tree sweeps (./sweep.ts), held from both ends: a test
// of a file that loads the tree runs under SWEEP_TIMEOUT_MS, and every module
// that globs the source tree raw calls boundSweep() at module scope.
import ts from "typescript";
import { describe, expect, it } from "vitest";

import { allFiles } from "@/__tests__/a11y/_scanner";

import { SWEEP_TIMEOUT_MS } from "./sweep";

// A Vite glob whose first pattern opens on the whole source tree (/src/**):
// any type argument (glob<Record<string, string>>) is skipped up to the
// paren, and the pattern may open an array, on the same line or the next.
const TREE_GLOB = /import\.meta\.glob[^(]*\(\s*\[?\s*["'`]\/src\/\*\*/;

// The name a top-level call statement calls: `f()`, or the base of `it.each(…)(…)`
// and `test.skip(…)`.
function calleeName(expr: ts.Expression): string | undefined {
  let callee = expr;
  for (;;) {
    if (ts.isCallExpression(callee)) callee = callee.expression;
    else if (ts.isPropertyAccessExpression(callee)) callee = callee.expression;
    else break;
  }
  return ts.isIdentifier(callee) ? callee.text : undefined;
}

// A module is bound when a module-scope boundSweep() statement runs before its
// first top-level it()/test(): Vitest reads testTimeout when a test registers,
// and a top-level test registers as the module body runs. A describe() may come
// first, since its factory runs after the module body. Read on the AST, so a
// commented-out or nested call does not pass for it.
function isBound(path: string, src: string): boolean {
  const sf = ts.createSourceFile(path, src, ts.ScriptTarget.Latest, false, ts.ScriptKind.TSX);
  for (const stmt of sf.statements) {
    if (!ts.isExpressionStatement(stmt) || !ts.isCallExpression(stmt.expression)) continue;
    const name = calleeName(stmt.expression);
    if (name === "boundSweep") return true;
    if (name === "it" || name === "test") return false;
  }
  return false;
}

// The fixtures below spell the glob through this name, so this file's own
// text holds no tree glob for the guard to find.
const GLOB = ["import", "meta", "glob"].join(".");

describe("whole-tree sweeps run under the sweep bound", () => {
  it("registers the tests of a file that loads the tree under SWEEP_TIMEOUT_MS", ({ task }) => {
    // This file loads the tree through _scanner.ts, which Vite's glob leaves
    // out of its own list: the bound this test runs under is the witness that
    // _scanner.ts raises it.
    expect(task.timeout).toBe(SWEEP_TIMEOUT_MS);
  });

  it("calls boundSweep() beside every raw glob of the source tree", () => {
    const globbing = allFiles.filter(([, src]) => TREE_GLOB.test(src));
    // lostRuns.sweep.test.ts and EnableRepoPanel.guarantee.test.tsx glob the
    // tree themselves; a glob the pattern stopped seeing would empty this list.
    expect(globbing.map(([path]) => path)).toEqual(
      expect.arrayContaining([
        "/src/lib/lostRuns.sweep.test.ts",
        "/src/views/teams/tabs/__tests__/EnableRepoPanel.guarantee.test.tsx",
      ]),
    );
    const unbound = globbing.filter(([path, src]) => !isBound(path, src)).map(([path]) => path);
    expect(unbound, "a module globs the source tree without boundSweep() at module scope").toEqual([]);
  });

  it("sees a tree glob in its typed, array and multiline forms, and not a relative glob", () => {
    const tree = [
      `${GLOB}("/src/**/*.ts")`,
      `${GLOB}<string>("/src/**")`,
      `${GLOB}<Record<string, string>>("/src/**")`,
      `${GLOB}(["/src/**/*.ts", "!**/*.test.ts"])`,
      `${GLOB}(\n  [\n    "/src/**/*.ts",\n  ],\n)`,
    ];
    expect(tree.filter((src) => !TREE_GLOB.test(src))).toEqual([]);
    expect(TREE_GLOB.test(`${GLOB}("./**/*Page.tsx")`)).toBe(false);
  });

  it("counts only a module-scope boundSweep() that runs before the first top-level test", () => {
    const glob = `const R = ${GLOB}("/src/**", {});`;
    // A glob string holds the `/*` of a comment opener; the real call after it still binds.
    expect(isBound("bound.ts", `${glob}\nboundSweep();\nconst y = 1; /* a */`)).toBe(true);
    // describe() factories run after the module body, so the call may follow one.
    expect(isBound("describe.ts", `${glob}\ndescribe("d", () => {});\nboundSweep();`)).toBe(true);
    expect(isBound("comment.ts", `${glob}\n/*\nboundSweep();\n*/`)).toBe(false);
    expect(isBound("order.ts", `${glob}\nit("t", () => {});\nboundSweep();`)).toBe(false);
    expect(isBound("each.ts", `${glob}\nit.each([1])("t", () => {});\nboundSweep();`)).toBe(false);
    expect(isBound("nested.ts", `${glob}\nfunction f() {\n  boundSweep();\n}`)).toBe(false);
  });
});
