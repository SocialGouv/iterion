import { describe, expect, it } from "vitest";
import { absoluteTargetHits } from "./_adminNestLinks";
import { hit, isIntrinsic, jsxClassed } from "./_jsx";
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

  it("writes no absolute path literal under views/admin — the console is a nested router", () => {
    // App.tsx mounts the console with `<Route path="/admin" nest>`, so wouter
    // resolves every href/navigate inside it against /admin: a literal
    // "/admin/users" becomes /admin/admin/users and a literal "/orgs/<id>"
    // becomes /admin/orgs/<id>. Inside the console a target comes from the
    // route table (adminRoutes.ts, paths relative to /admin); a target
    // OUTSIDE the console goes through studioRoute() (lib/scope.ts), which
    // drops the nested base and re-applies the studio's.
    //
    // The detector is _adminNestLinks.absoluteTargetHits, run here over the
    // real tree and over hand-written bypass spellings in
    // adminNestLinks.test.ts — the same function in both, so what that file
    // proves is what this gate enforces.
    const hits = absoluteTargetHits(files);
    if (hits.length) {
      throw new Error(
        `an absolute path literal inside the /admin nest resolves under /admin — use the adminRoutes table (relative) or studioRoute() to leave the console:\n${hits.join("\n")}`,
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
        // Same-line exemption keys on the JSX element (<Spinner), not
        // the bare word — a comment or import mentioning Spinner must
        // not launder a raw literal past the trap.
        if (RE.test(line) && !line.includes("<Spinner")) {
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

  // The four guards below read the JSX (per element, per tag, every static
  // className shape) through _jsx.ts — a regex over `className="…"` saw one
  // shape of four and let ~30 sites through.

  it("never dims an element with a static opacity — tokens for text, a prefixed variant for inactive states", () => {
    // `opacity-70` on a <span>/<code>/<li>, a text colour with an alpha
    // (`text-fg-subtle/70`), an inline `style={{ opacity: .75 }}` or a
    // `hover:opacity-80` all lower the text's contrast with the alpha (the
    // audit measured 4.39:1 and below, RGAA 3.2). De-emphasised text is a
    // tier (text-fg-muted / text-fg-subtle); an inactive control or row is
    // `disabled:opacity-60` / `data-[disabled]:opacity-60` with the
    // matching attribute — prefixed by a state, so exempt here and explicit
    // in the DOM. Every step of the scale dims (opacity-5 … opacity-95) and
    // so does an arbitrary value (`opacity-[.6]`, `opacity-(--x)`); only
    // `opacity-0` (a reveal that ends at `group-hover:opacity-100`) and 100
    // are not a dimming.
    const re =
      /(?<![\w:-])(hover:)?opacity-(?:[1-9]\d?|\[[^\]]*\]|\([^)]*\))(?![\w-])|(?<![\w:-])text-(fg|accent|danger|warning|success|info|live|persona|node)[\w-]*\/(?:\d+|\[[^\]]*\])(?![\w-])/;
    const hits = jsxClassed()
      .filter((e) => isIntrinsic(e.tag) && (re.test(e.classes) || e.styleOpacity !== undefined))
      .map((e) => hit(e));
    if (hits.length) {
      throw new Error(
        `static opacity on an element is banned (design-system.md § Don'ts) — text-fg-muted / text-fg-subtle for text, disabled: / data-[disabled]: for inactive states:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("never animates opacity on an element that carries a text colour (RGAA 3.2)", () => {
    // `animate-pulse` / `animate-ping` run an opacity animation; at the
    // trough the element's text colour drops under the 3:1 floor — the
    // animated twin of the static-opacity ban above. The static ban
    // targets the onAccent pair (a light text on a solid colour, the
    // pairing with no contrast headroom); softer pairs pass at the trough
    // and their remaining motion is governed by the reduced-motion gate.
    const re = /(?<![\w:-])animate-(?:pulse|ping)(?![\w-])/;
    const textColour = /(?:^|\s)(?:hover:|focus-visible:)?text-fg-onAccent(?![\w-])/;
    const hits = jsxClassed()
      .filter((e) => isIntrinsic(e.tag) && re.test(e.classes) && textColour.test(e.classes))
      .map((e) => hit(e, "opacity animation + text colour"));
    if (hits.length) {
      throw new Error(
        `an opacity animation on a text-coloured element drops the label under 3:1 at the trough (RGAA 3.2) — keep the pulse icon-only, or drop it:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("no native control or control primitive draws its boundary with a chrome border token (RGAA 3.3)", () => {
    // An <input>, <textarea>, <select> — or a className handed to one of
    // the control primitives, which overrides theirs (Tailwind emits
    // same-property utilities in alphabetical order) — carrying
    // border-strong / -default / -subtle (under 2.5:1 against the surface)
    // has no perceptible boundary. Chrome around the control (its popover,
    // its card) keeps its hairlines — only the control's own tag is read.
    const hits = jsxClassed()
      .filter(
        (e) =>
          /^(input|textarea|select|Input|Textarea|Select|TagInput|Combobox|MarkdownPreview)$/.test(e.tag) &&
          /\bborder-border-(strong|default|subtle)\b/.test(e.classes),
      )
      .map((e) => hit(e));
    if (hits.length) {
      throw new Error(
        `a control with a chrome border token (design-system.md § Don'ts) — use border-border-control:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("every ui/ file rendering a native text control draws its boundary with border-control (RGAA 3.3)", () => {
    // The inventory is read from the code, not listed: a new primitive that
    // copies border-strong lands here. File-level on purpose — TagInput's
    // inner <input> is borderless, its wrapper draws the control edge.
    // Native checkboxes and radios draw their own box (appearance: auto),
    // so a border class on them is inert and they are not held to it —
    // the element's `type` attribute, read from the JSX, tells them apart.
    const textControlFiles = new Set(
      jsxClassed()
        .filter(
          (e) =>
            e.path.includes("/components/ui/") &&
            (/^(textarea|select)$/.test(e.tag) || (e.tag === "input" && e.inputType !== "checkbox" && e.inputType !== "radio")),
        )
        .map((e) => e.path),
    );
    // The classes are read from the JSX, not the file text: a comment
    // naming border-control draws no boundary.
    const classesOf = new Map<string, string>();
    for (const e of jsxClassed()) classesOf.set(e.path, `${classesOf.get(e.path) ?? ""} ${e.classes}`);
    const controls = [...textControlFiles];
    expect(controls.length).toBeGreaterThanOrEqual(6);
    const missing = controls.filter((p) => !/\bborder-border-control\b/.test(classesOf.get(p) ?? ""));
    const strong = controls.filter((p) => /\bborder-border-strong\b/.test(classesOf.get(p) ?? ""));
    expect(missing, "control files without border-border-control").toEqual([]);
    expect(strong, "control files still using border-border-strong").toEqual([]);
  });

  it("text-accent-fg / text-fg-onAccent only sit on bg-accent, a bg-<severity>-solid or a black scrim", () => {
    // Both tokens are white: on bg-accent-soft, bg-accent/20 or a bare
    // surface they read 1.0-1.5:1 in the light theme (RGAA 3.2), and on
    // a plain severity (the text colour: dark red-400 … cyan-500) 2.2-2.8:1.
    // Accent text on a surface is text-accent-text; a white label sits on
    // the -solid twin, or on the black scrim of a chip inside an accent
    // row. The background sits under the same state prefix as the text,
    // or under none (`focus:bg-accent focus:text-fg-onAccent` is one
    // state; a `hover:bg-accent` under an unprefixed white text leaves
    // the resting state white on nothing). Judged per variant: the pair
    // must hold in every shape the className can take.
    const WHITE = /(?<![\w-])((?:[\w-]+:)*)text-(?:accent-fg|fg-onAccent)(?![\w-])/g;
    const SOLID = String.raw`bg-(?:accent|(?:danger|warning|success|info)-solid)(?![\w/-])|bg-black\/`;
    const solidUnder = (v: string, prefix: string) =>
      new RegExp(`(?<![\\w:-])${prefix.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?:${SOLID})`).test(v);
    const hits = jsxClassed().flatMap((e) =>
      e.variants
        .filter((v) =>
          [...v.matchAll(WHITE)].some(([, prefix = ""]) => !solidUnder(v, prefix) && !(prefix && solidUnder(v, ""))),
        )
        .map((v) => hit(e, v)),
    );
    if (hits.length) {
      throw new Error(
        `white accent-fg text off bg-accent / bg-<severity>-solid (design-system.md § Don'ts) — text-accent-text on a surface, bg-<severity>-solid under a white label:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("text-accent-text never sits on a bg-accent/NN alpha tint — bg-accent-soft is the tint", () => {
    // The tint under accent text is the accent-soft token: one pair, held
    // ≥ 4.5:1 by the contrast audit (5.0+ over the light panels). An
    // ad-hoc alpha is a pair no row measures — accent/20 read 4.4-4.6:1
    // over the light panels (RGAA 3.2) — so every alpha is refused, the
    // faint ones that happen to pass included. Judged per variant, state
    // prefixes included (a hover:bg-accent/30 is the hovered pair).
    const hits = jsxClassed().flatMap((e) =>
      e.variants
        .filter((v) => /\btext-accent-text\b/.test(v) && /(?<![\w-])(?:[\w-]+:)?bg-accent\/\d+\b/.test(v))
        .map((v) => hit(e, v)),
    );
    if (hits.length) {
      throw new Error(
        `text-accent-text on a bg-accent/NN alpha tint (design-system.md § Don'ts) — use bg-accent-soft:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("no className overflows the reader's variant cap — a collapsed element is judged on its union", () => {
    // Past MAX_VARIANTS the reader collapses the product to the union, and
    // a pairing rule then sees classes from different branches together
    // (a false pair, or a missed one). Named here so an overflow is a
    // red test, not a silent change of what the rules above judge.
    const hits = jsxClassed()
      .filter((e) => e.collapsed)
      .map((e) => hit(e));
    if (hits.length) {
      throw new Error(
        `a className's variant product passed the reader's cap and collapsed to its union — split the className, or raise MAX_VARIANTS in _jsx.ts:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  it("text-fg-subtle never sits on a severity or accent -soft tint", () => {
    // fg-subtle holds AA on the grey surfaces; on a -soft tint it reads
    // 4.0-4.2:1 over the light panels, and under 4.5 in dark on live-soft
    // or wherever the tint sits on surface-2. Secondary text on a tinted
    // banner is text-fg-muted (or the tint's own -fg). Judged per variant.
    const hits = jsxClassed().flatMap((e) =>
      e.variants
        .filter((v) => /\bbg-(danger|warning|success|info|live|accent)-soft\b/.test(v) && /\btext-fg-subtle\b/.test(v))
        .map((v) => hit(e, v)),
    );
    if (hits.length) {
      throw new Error(
        `text-fg-subtle on a -soft tint (design-system.md § Don'ts) — use text-fg-muted:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });
});
