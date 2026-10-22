// Reads the colour tokens of app.css the way the cascade ships them, and
// refuses what it cannot model. NOT a test — imported by
// tokens-mirror.test.ts (over the real app.css) and exercised on
// hand-written CSS by css-tokens.test.ts.
//
// app.css declares every `--color-*` in exactly three top-level blocks:
// `@theme` (the defaults Tailwind resolves — the dark palette), then
// `:root[data-theme="dark"]` and `:root[data-theme="light"]`, which the
// theme toggle selects. A declaration anywhere else — a plain `:root`, an
// `@layer`, an `@media`, a theme block nested in an at-rule — or one
// marked `!important` is a value the mirror would not read and the
// cascade would ship (or drop): it is named as a problem, never read
// around. A declaration nested inside a theme block (an `@media` or a
// selector the block wraps — Lightning CSS hoists the at-rule to the top
// level, so it is not the block's own value either) is a problem too.
// Comments are blanked first (newlines kept, so line numbers hold): a
// `/* was: --color-x: … */` is not a declaration; a string literal is
// skipped whole, so a `content: "}"` does not close a block.

export interface TokenBlocks {
  theme: Record<string, string>;
  dark: Record<string, string>;
  light: Record<string, string>;
  /** What the reader refuses to model, one line each; empty when app.css has the expected shape. */
  problems: string[];
}

const DECL = /--color-([\w-]+)\s*:\s*([^;}]+)/g;
const IMPORTANT = /!\s*important\b/i;

/** The body with every nested `{…}` blanked (own declarations), and the nested parts alone. */
function split(body: string): { own: string; nested: string } {
  let own = "";
  let nested = "";
  let depth = 0;
  for (const ch of body) {
    if (ch === "{") depth++;
    if (depth === 0) {
      own += ch;
      nested += ch === "\n" ? ch : " ";
    } else {
      own += ch === "\n" ? ch : " ";
      nested += ch;
    }
    if (ch === "}") depth--;
  }
  return { own, nested };
}

/** The index just past a string literal opened at `i` (backslash escapes honoured; an unterminated string ends at the line). */
function skipString(css: string, i: number): number {
  const quote = css[i];
  for (let j = i + 1; j < css.length; j++) {
    const ch = css[j];
    if (ch === "\\") j++;
    else if (ch === quote || ch === "\n") return j;
  }
  return css.length - 1;
}

function blockFor(selector: string): "theme" | "dark" | "light" | undefined {
  if (selector === "@theme") return "theme";
  const m = selector.match(/^:root\[data-theme=["']?(dark|light)["']?\]$/);
  return m?.[1] === "dark" ? "dark" : m?.[1] === "light" ? "light" : undefined;
}

export function tokenBlocks(cssText: string): TokenBlocks {
  const css = cssText.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "));
  const out: TokenBlocks = { theme: {}, dark: {}, light: {}, problems: [] };
  const seen = new Set<string>();
  const lineOf = (i: number) => css.slice(0, i).split("\n").length;
  let depth = 0;
  let selector = "";
  let open = -1;
  let name = "";
  for (let i = 0; i < css.length; i++) {
    const c = css[i];
    if (c === '"' || c === "'") {
      const end = skipString(css, i);
      if (depth === 0) selector += css.slice(i, end + 1);
      i = end;
      continue;
    }
    if (c === "{") {
      if (depth === 0) {
        open = i;
        name = selector.trim().replace(/\s+/g, " ");
        selector = "";
      }
      depth++;
    } else if (c === "}") {
      depth--;
      if (depth === 0) {
        const target = blockFor(name);
        if (target) seen.add(target);
        const body = css.slice(open + 1, i);
        const { own, nested } = target ? split(body) : { own: body, nested: "" };
        for (const m of nested.matchAll(DECL)) {
          const line = lineOf(open + 1 + (m.index ?? 0));
          out.problems.push(`line ${line}: --color-${m[1] ?? ""} is declared in a rule nested inside \`${name}\` — not the block's own value`);
        }
        for (const m of own.matchAll(DECL)) {
          const key = m[1] ?? "";
          const value = (m[2] ?? "").trim();
          const line = lineOf(open + 1 + (m.index ?? 0));
          if (IMPORTANT.test(value)) {
            out.problems.push(`line ${line}: --color-${key} is !important — the reader holds the cascade's last declaration, an !important one would beat it`);
          }
          if (!target) {
            out.problems.push(`line ${line}: --color-${key} is declared in \`${name}\` — tokens live in @theme and the two top-level :root[data-theme] blocks only`);
            continue;
          }
          out[target][key] = value;
        }
      }
    } else if (depth === 0) {
      // A brace-less statement (@import, @custom-variant) ends at `;`.
      selector = c === ";" ? "" : selector + c;
    }
  }
  for (const b of ["theme", "dark", "light"] as const) {
    if (!seen.has(b)) out.problems.push(`no top-level ${b === "theme" ? "@theme" : `:root[data-theme="${b}"]`} block`);
  }
  return out;
}
