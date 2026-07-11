import { describe, expect, it } from "vitest";
import { files, scan, scanWhole } from "./_scanner";

// Source-discipline regression traps. Each scans every source module as raw
// text (Vite glob, no node:fs — works in vitest's node + jsdom envs) and
// asserts a banned pattern stays at zero. Companion to broken-classes.test.ts
// (phantom tokens) and the design-system.md "Don'ts" table.
//
// These pin invariants the codebase already satisfies, so a regression shows
// up as a failing unit test instead of silent drift:
//   - no window.confirm/alert            -> use the useConfirm() hook
//   - no raw hex colour literals as JS/CSS-in-JS values
//                                        -> a token (lib/constants.ts) or a
//                                           Tailwind utility
//
// Adding a real token to app.css never requires touching this file.

describe("source discipline", () => {
  it("scans a non-trivial number of source files", () => {
    // Guards against a glob-scope regression silently emptying the scan.
    expect(files.length).toBeGreaterThan(150);
  });

  it("never calls window.confirm() / window.alert() — use the useConfirm() hook", () => {
    const hits = scan(/\bwindow\.(confirm|alert)\s*\(/);
    if (hits.length) {
      throw new Error(
        `window.confirm/alert is banned (design-system.md § Don'ts) — use useConfirm():\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("has no raw hex colour literals as values — use a token or Tailwind utility", () => {
    // A hex string used as an object / style value: `color: "#3b82f6"`,
    // `backgroundColor: '#fff'`. The `:` anchor excludes anchor hrefs
    // (`href="#main"`, which use `=`) and encoded data-URIs (`%23…`). The
    // canonical mirror lib/constants.ts is the one allowed home for raw
    // colour values (and it uses var() strings, not hex, today).
    const hits = scan(
      /:\s*["']#[0-9a-fA-F]{3,8}["']/,
      (path) => path.endsWith("/lib/constants.ts"),
    );
    if (hits.length) {
      throw new Error(
        `raw hex colour literal as a value is banned (design-system.md § Don'ts) — add a token to app.css and consume via lib/constants.ts or a Tailwind utility:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("has no legacy ${token}NN soft-bg interpolation — use softColor(token, pct)", () => {
    // `${color}22` only worked when `color` was an inline hex string; with the
    // var(--color-*) token strings it produces invalid CSS (var(--x)22) so the
    // background/border silently renders nothing. softColor() (color-mix) is
    // the correct replacement. The lookahead pins the 2 hex digits to a
    // closing quote/backtick (the template-literal-as-value shape) to avoid
    // matching incidental `${x}ed`-style text. lib/constants.ts documents the
    // legacy pattern in a comment, so it is allow-listed.
    const hits = scan(
      /\$\{[^}]+\}[0-9a-fA-F]{2}(?=["'\x60])/,
      (path) => path.endsWith("/lib/constants.ts"),
    );
    if (hits.length) {
      throw new Error(
        `legacy \${token}NN soft-bg is banned — use softColor(token, pct):\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("has no legacy color + \"NN\" hex-alpha concat — use softColor(token, pct)", () => {
    // Sibling of the `${token}NN` guard above, for the concat form:
    // `color + "44"`. Same silent-failure mode — when `color` is a
    // var(--color-*) string, `var(--x)44` is invalid CSS and the
    // background/border renders transparent. softColor() (color-mix) is
    // the correct replacement. The regex anchors on `+` followed by a
    // quoted 2-hex-digit literal so it doesn't match incidental string
    // concatenations like `"foo" + "0a"` in unrelated code. lib/constants.ts
    // is the documented home for softColor and any related legacy commentary,
    // so it is allow-listed (matching the sibling guard).
    const hits = scan(
      /\+\s*["'\x60][0-9a-fA-F]{2}["'\x60]/,
      (path) => path.endsWith("/lib/constants.ts"),
    );
    if (hits.length) {
      throw new Error(
        `legacy color + "NN" hex-alpha concat is banned — use softColor(token, pct):\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("uses the Checkbox / Radio primitives, not raw <input type=checkbox|radio>", () => {
    // ui/Checkbox + ui/Radio own the only native checkbox/radio inputs (token
    // border, brand-accent fill, free keyboard/SR semantics). `[^>]*` spans
    // newlines so a multiline `<input …>` tag is still caught; anchoring on
    // `<input` means a `type="radio"` *prop* on a component (e.g. OptionRow)
    // is correctly ignored.
    const RE = /<input\b[^>]*\btype\s*=\s*["'](checkbox|radio)["']/;
    const hits = scanWhole(RE, (path) => /\/ui\/(Checkbox|Radio)\.tsx$/.test(path));
    if (hits.length) {
      throw new Error(
        `raw <input type=checkbox|radio> is banned — use <Checkbox>/<Radio>/<RadioGroup>:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("uses the Select primitive, not raw <select>", () => {
    // ui/Select.tsx owns the only native <select> (token border, themed
    // chevron overlay, focus ring, size/fit variants). Inspector forms route
    // through FormField's SelectField → <Select>; a new raw <select> elsewhere
    // bypasses all of that. Line-anchored so prose mentions of "<select>" in
    // comments don't trip it.
    const hits = scan(/^\s*<select\b/, (path) => /\/ui\/Select\.tsx$/.test(path));
    if (hits.length) {
      throw new Error(
        `raw <select> is banned — use <Select> from @/components/ui/Select:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("uses the Table primitive, not raw <table>", () => {
    // ui/Table owns the canonical data-table markup (overflow wrapper,
    // mandatory sr-only caption for RGAA 5.4/5.5, scope=col headers for
    // 5.7, densities, TableSkeleton). Allow-listed exceptions, each with
    // the RGAA fixes applied in place:
    //   - MarkdownText.tsx renders markdown-authored tables;
    //   - RunListView.tsx toggles `hidden sm:table` display on the table
    //     element itself (mobile renders cards instead) — incompatible
    //     with the primitive's wrapper;
    //   - ArtifactFilesPanel.tsx needs a sticky thead inside its own
    //     scroll container.
    const hits = scan(/^\s*<table\b/, (path) =>
      /\/(ui\/Table|conversation\/MarkdownText|Runs\/RunListView|Runs\/ArtifactFilesPanel)\.tsx$/.test(
        path,
      ),
    );
    if (hits.length) {
      throw new Error(
        `raw <table> is banned — use <Table> from @/components/ui/Table (caption + scope=col built in):\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("renders loading through BootLoading/MainSpinner/Spinner/Skeleton, not raw Loading… literals", () => {
    // A rendered "Loading…" literal (JSX text or string value) bypasses the
    // blessed loading vocabulary — shared/BootLoading (pre-shell boot),
    // shared/MainSpinner (route Suspense inside the shell), ui/Spinner
    // (inline section/panel), ui/Skeleton + TableSkeleton (known layout) —
    // shipping text with no motion affordance and no role=status semantics.
    // `EmptyState message="Loading…"` is the classic misuse: EmptyState is
    // for EMPTY content, not a pending fetch.
    //
    // Same-line exemption: a visible "Loading…" label is fine when it sits
    // next to a <Spinner /> on the same line (the canonical spinner+text
    // composition, e.g. the Toolbar file-loading pill).
    //
    // File allowlist (each deliberately keeps the literal):
    //   - ui/Spinner.tsx — doc comment mentions the literal;
    //   - shared/BootLoading.tsx, shared/MainSpinner.tsx — the blessed
    //     compositions themselves (Spinner + <span>Loading…</span>);
    //   - views/auth/AcceptInvitation.tsx — pre-shell status screens already
    //     composed as Spinner + text across two lines;
    //   - views/Skills.tsx — "Loading…" as a textarea placeholder= attribute,
    //     not a rendered literal.
    const ALLOW =
      /\/(ui\/Spinner|shared\/BootLoading|shared\/MainSpinner|views\/auth\/AcceptInvitation|views\/Skills)\.tsx$/;
    const RE = /(^\s*|>\s*|["'])Loading…/;
    const hits: string[] = [];
    for (const [path, src] of files) {
      if (ALLOW.test(path)) continue;
      src.split("\n").forEach((line, i) => {
        if (RE.test(line) && !line.includes("Spinner")) {
          hits.push(`${path}:${i + 1}  ${line.trim().slice(0, 100)}`);
        }
      });
    }
    if (hits.length) {
      throw new Error(
        `raw Loading… literal is banned — use BootLoading, MainSpinner, <Spinner label="…"/>, or Skeleton/TableSkeleton:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("uses the semantic type scale, not text-[Npx] sizes that have a token", () => {
    // 10/11/12/13/14/16px have tokens (text-caption/micro/body/label/title/
    // display). text-[8px]/[9px] are below the scale floor (no token, used in
    // pips/dense badges) and intentionally allowed.
    const hits = scan(/text-\[(10|11|12|13|14|16)px\]/);
    if (hits.length) {
      throw new Error(
        `text-[Npx] with a token equivalent is banned — use text-caption/micro/body/label/title/display:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });
});
