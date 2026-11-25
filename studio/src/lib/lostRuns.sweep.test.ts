// Every raw transport call of the studio is accounted for against the
// lost-run registry (@/lib/lostRuns), read on the TypeScript AST — comments
// and strings cannot hide or fake a call. A raw call is fetch (bare, on
// window / globalThis / self, through an alias, .call/.apply), or new
// WebSocket / EventSource / XMLHttpRequest (bare or on a global). A call is
// guarded when a function enclosing it calls a registry check before it.
// A JSX src= (or …Src) attribute that spells a run path, and an href= (or
// …Href) attribute that spells a run API path, WITHOUT a run-URL builder
// (apiURL and the builders on it, withExpectedBirth) are counted too: the
// browser loads or follows them with no request function in between, so
// only a builder puts the page's store and the run's expected birth on them
// — none may skip them. An href to the studio's own /runs/<id> route is a
// navigation, not a run resource.
//
// Each module's counts are pinned: a new raw call, a guard removed, or a
// run-path src/href outside the builders fails here until it is classified.
//
// LOST_RUNS_SWEEP_OVERLAY names a JSON map of source path → replacement file,
// substituted before the scan the way `go test -overlay` does, so a mutant is
// scanned without editing the tree.
import { readFileSync } from "node:fs";

import ts from "typescript";
import { describe, expect, it } from "vitest";

import { boundSweep } from "@/test/sweep";

boundSweep();

const RAW = import.meta.glob("/src/**/*.{ts,tsx}", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

const overlayFile = process.env.LOST_RUNS_SWEEP_OVERLAY;
const overlay: Record<string, string> = overlayFile
  ? (JSON.parse(readFileSync(overlayFile, "utf8")) as Record<string, string>)
  : {};

const files = Object.entries(RAW)
  .filter(([path]) => !path.includes("/__tests__/") && !/\.test\.tsx?$/.test(path))
  .map(([path, src]) => {
    const replacement = overlay[path];
    return [path, replacement ? readFileSync(replacement, "utf8") : src] as const;
  });

interface Counts {
  unguarded: number;
  guarded: number;
  src: number;
}

const GLOBALS = new Set(["window", "globalThis", "self"]);
const CTORS = new Set(["WebSocket", "EventSource", "XMLHttpRequest"]);
const FETCH = new Set(["fetch"]);
const GUARDS = new Set(["assertRunReachable", "lostRunAddressedBy", "isRunLost"]);
// A src= expression spells a run path; it is built when it calls a builder
// that puts the expected birth on it (@/api/runs apiURL and the builders
// on it, @/lib/lostRuns withExpectedBirth).
const RUN_PATH = /\/runs\//;
const RUN_API_PATH = /\/api\/(ws\/)?runs\/|\b(BASE_URL|apiBase\(\))\b[^;]*\/runs\//;
const BUILDERS = /\b(attachmentURL|artifactFileURL|workspaceFileURL|runPreviewURL|runBrowserAttachURL|apiURL|withExpectedBirth)\(/;

function strip(e: ts.Expression): ts.Expression {
  while (ts.isParenthesizedExpression(e) || ts.isAsExpression(e) || ts.isNonNullExpression(e)) e = e.expression;
  return e;
}

// onGlobal names the global a reference spells — bare, window.X,
// globalThis.X, self.X or self["X"] — when it is one of names.
function onGlobal(e: ts.Expression, names: Set<string>): string | null {
  e = strip(e);
  if (ts.isIdentifier(e)) return names.has(e.text) ? e.text : null;
  if (ts.isPropertyAccessExpression(e) && ts.isIdentifier(e.expression) && GLOBALS.has(e.expression.text)) {
    return names.has(e.name.text) ? e.name.text : null;
  }
  if (
    ts.isElementAccessExpression(e) &&
    ts.isIdentifier(e.expression) &&
    GLOBALS.has(e.expression.text) &&
    ts.isStringLiteral(e.argumentExpression)
  ) {
    return names.has(e.argumentExpression.text) ? e.argumentExpression.text : null;
  }
  return null;
}

function scanSource(path: string, src: string): Counts {
  const sf = ts.createSourceFile(
    path,
    src,
    ts.ScriptTarget.Latest,
    true,
    path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
  );
  const fetchNames = new Set(["fetch"]);
  // A fetch reference: fetch / window.fetch / an alias, or one of them bound.
  const isFetchRef = (e: ts.Expression): boolean => {
    e = strip(e);
    if (ts.isIdentifier(e)) return fetchNames.has(e.text);
    if (onGlobal(e, FETCH)) return true;
    if (ts.isCallExpression(e)) {
      const callee = strip(e.expression);
      return ts.isPropertyAccessExpression(callee) && callee.name.text === "bind" && isFetchRef(callee.expression);
    }
    return false;
  };
  // Aliases (const f = fetch; const g = window.fetch.bind(window)), to a fixpoint.
  for (let changed = true; changed; ) {
    changed = false;
    const visit = (n: ts.Node): void => {
      if (ts.isVariableDeclaration(n) && ts.isIdentifier(n.name) && n.initializer && !fetchNames.has(n.name.text)) {
        if (isFetchRef(n.initializer)) {
          fetchNames.add(n.name.text);
          changed = true;
        }
      }
      ts.forEachChild(n, visit);
    };
    visit(sf);
  }
  // Initializers of local identifiers, for src={someUrl}.
  const initializers = new Map<string, string>();
  const collect = (n: ts.Node): void => {
    if (ts.isVariableDeclaration(n) && ts.isIdentifier(n.name) && n.initializer) {
      initializers.set(n.name.text, n.initializer.getText(sf));
    }
    ts.forEachChild(n, collect);
  };
  collect(sf);

  const guards: ts.CallExpression[] = [];
  const raw: ts.Node[] = [];
  let srcCount = 0;
  const walk = (n: ts.Node): void => {
    if (ts.isCallExpression(n)) {
      const callee = strip(n.expression);
      if (ts.isIdentifier(callee) && GUARDS.has(callee.text)) guards.push(n);
      const viaCallApply =
        ts.isPropertyAccessExpression(callee) &&
        (callee.name.text === "call" || callee.name.text === "apply") &&
        isFetchRef(callee.expression);
      const bound = ts.isPropertyAccessExpression(callee) && callee.name.text === "bind";
      if (!bound && (isFetchRef(callee) || viaCallApply)) raw.push(n);
    }
    if (ts.isNewExpression(n) && onGlobal(n.expression, CTORS)) raw.push(n);
    const attr = ts.isJsxAttribute(n) ? n.name.getText(sf) : "";
    const isSrc = attr === "src" || attr.endsWith("Src");
    const isHref = attr === "href" || attr.endsWith("Href");
    if (ts.isJsxAttribute(n) && (isSrc || isHref) && n.initializer && ts.isJsxExpression(n.initializer)) {
      const expr = n.initializer.expression;
      if (expr) {
        const text = expr.getText(sf);
        const target = ts.isIdentifier(strip(expr)) ? (initializers.get(text) ?? "") : text;
        const path = isSrc ? RUN_PATH : RUN_API_PATH;
        const spellsRun = path.test(text) || path.test(target);
        const built = BUILDERS.test(text) || BUILDERS.test(target);
        if (spellsRun && !built) srcCount++;
      }
    }
    ts.forEachChild(n, walk);
  };
  walk(sf);

  let guarded = 0;
  for (const call of raw) {
    let isGuarded = false;
    for (let p: ts.Node | undefined = call.parent; p && !isGuarded; p = p.parent) {
      if (!ts.isFunctionLike(p)) continue;
      const fn = p;
      isGuarded = guards.some((g) => g.pos >= fn.pos && g.end <= fn.end && g.end <= call.pos);
    }
    if (isGuarded) guarded++;
  }
  return { unguarded: raw.length - guarded, guarded, src: srcCount };
}

// Modules whose raw calls address no run path (unguarded), modules whose
// run-addressed raw call is checked first (guarded), and run-path src=
// attributes outside the builders (none: each would load without the run's
// expected birth).
const EXPECTED: Record<string, Counts & { why: string }> = {
  "/src/api/client.ts": { unguarded: 1, guarded: 1, src: 0, why: "session refresh; every request through sendRequest" },
  "/src/lib/lostRuns.ts": { unguarded: 0, guarded: 1, src: 0, why: "runFetch" },
  "/src/hooks/useRunWebSocket.ts": { unguarded: 0, guarded: 1, src: 0, why: "the run socket" },
  "/src/components/Runs/RunShellPanel.tsx": { unguarded: 0, guarded: 1, src: 0, why: "the shell socket" },
  "/src/lib/cdpClient.ts": { unguarded: 0, guarded: 1, src: 0, why: "the CDP socket" },
  "/src/api/auth.ts": { unguarded: 1, guarded: 0, src: 0, why: "auth endpoints" },
  "/src/api/bots.ts": { unguarded: 1, guarded: 0, src: 0, why: "bot uploads" },
  "/src/api/byok.ts": { unguarded: 1, guarded: 0, src: 0, why: "key endpoints" },
  "/src/api/configShare.ts": { unguarded: 1, guarded: 0, src: 0, why: "share links" },
  "/src/api/memory.ts": { unguarded: 1, guarded: 0, src: 0, why: "memory documents" },
  "/src/api/plugins.ts": { unguarded: 2, guarded: 0, src: 0, why: "plugin endpoints" },
  "/src/api/signOutMachine.ts": { unguarded: 1, guarded: 0, src: 0, why: "logout" },
  "/src/api/workspaceHandoff.ts": { unguarded: 1, guarded: 0, src: 0, why: "workspace handoffs: no run path; the body carries each run's birth" },
  "/src/api/ws.ts": { unguarded: 1, guarded: 0, src: 0, why: "the file-watcher socket (/api/ws)" },
  "/src/api/dispatcher.ts": { unguarded: 4, guarded: 0, src: 0, why: "the dispatcher socket" },
  "/src/api/runs/uploads.ts": { unguarded: 1, guarded: 0, src: 0, why: "attachment staging (/api/runs/uploads)" },
  "/src/auth/AuthContext.tsx": { unguarded: 1, guarded: 0, src: 0, why: "server info" },
  "/src/lib/desktopBridge.ts": { unguarded: 1, guarded: 0, src: 0, why: "desktop bridge" },
  "/src/lib/scope.ts": { unguarded: 2, guarded: 0, src: 0, why: "pane ws info and tickets" },
};

describe("raw transport calls and lost runs", () => {
  it("scans the source tree", () => {
    expect(files.length).toBeGreaterThan(150);
  });

  it("finds every raw call spelling and the guard before it", () => {
    const cases: [string, Counts][] = [
      ["fetch(u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["window.fetch(u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["globalThis.fetch(u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["self['fetch'](u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["const f = fetch; f(u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["const g = window.fetch.bind(window); g(u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["fetch.call(window, u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["new EventSource(u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["new window.WebSocket(u)", { unguarded: 1, guarded: 0, src: 0 }],
      ["new XMLHttpRequest()", { unguarded: 1, guarded: 0, src: 0 }],
      ["// fetch(u)\n/* new WebSocket(u) */ const s = 'fetch(u)'", { unguarded: 0, guarded: 0, src: 0 }],
      ["function f(u) { assertRunReachable(u); return fetch(u) }", { unguarded: 0, guarded: 1, src: 0 }],
      ["function f(u) { fetch(u); assertRunReachable(u) }", { unguarded: 1, guarded: 0, src: 0 }],
    ];
    for (const [code, want] of cases) {
      expect(scanSource("/scratch.ts", code), code).toEqual(want);
    }
    expect(scanSource("/scratch.tsx", "const v = <img src={`/api/runs/${id}/attachments/a`} />")).toEqual({ unguarded: 0, guarded: 0, src: 1 });
    expect(scanSource("/scratch.tsx", "const u = `/api/runs/${id}/preview`; const v = <iframe src={u} />")).toEqual({ unguarded: 0, guarded: 0, src: 1 });
    expect(scanSource("/scratch.tsx", "const u = apiURL(`/runs/${id}/log`); const v = <iframe src={u} />")).toEqual({ unguarded: 0, guarded: 0, src: 0 });
    expect(scanSource("/scratch.tsx", "const v = <img src={attachmentURL(id, n)} />")).toEqual({ unguarded: 0, guarded: 0, src: 0 });
    expect(scanSource("/scratch.tsx", "const v = <img src={logo} />")).toEqual({ unguarded: 0, guarded: 0, src: 0 });
    expect(scanSource("/scratch.tsx", "const v = <a href={`/api/runs/${id}/log`} download />")).toEqual({ unguarded: 0, guarded: 0, src: 1 });
    expect(scanSource("/scratch.tsx", "const v = <a href={`${BASE_URL}/runs/${id}/artifact-files/x`} />")).toEqual({ unguarded: 0, guarded: 0, src: 1 });
    expect(scanSource("/scratch.tsx", "const v = <Preview downloadHref={`/api/runs/${id}/attachments/a`} />")).toEqual({ unguarded: 0, guarded: 0, src: 1 });
    expect(scanSource("/scratch.tsx", "const v = <track src={`/api/runs/${id}/workspace-files/a.vtt`} captionsSrc={x} />")).toEqual({ unguarded: 0, guarded: 0, src: 1 });
    expect(scanSource("/scratch.tsx", "const v = <a href={artifactFileURL(id, p, { download: true })} />")).toEqual({ unguarded: 0, guarded: 0, src: 0 });
    expect(scanSource("/scratch.tsx", "const v = <a href={`/runs/${id}`}>open</a>")).toEqual({ unguarded: 0, guarded: 0, src: 0 });
  });

  it("pins every module's raw calls, guards and run-path src", () => {
    const actual: Record<string, Counts> = {};
    for (const [path, src] of files) {
      const c = scanSource(path, src);
      if (c.unguarded + c.guarded + c.src > 0) actual[path] = c;
    }
    const pinned: Record<string, Counts> = {};
    for (const [path, { unguarded, guarded, src }] of Object.entries(EXPECTED)) {
      pinned[path] = { unguarded, guarded, src };
    }
    expect(actual).toEqual(pinned);
  });
});
