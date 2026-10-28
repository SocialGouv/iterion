// Shared raw-text source scanner for the discipline guards (broken-classes +
// source-discipline). Loads every source module as raw text via Vite's glob
// (works in vitest's node + jsdom envs, no node:fs) and offers a per-line and
// a whole-file scan. NOT a test file — imported by the guards. Test files are
// excluded so a guard never matches itself.

const RAW = import.meta.glob("/src/**/*.{ts,tsx}", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

export const files: [string, string][] = Object.entries(RAW).filter(
  ([path]) => !path.includes("/__tests__/") && !/\.test\.tsx?$/.test(path),
);

/**
 * Every source module, TEST FILES INCLUDED — the scope a guard needs when the
 * pattern it hunts would live in a test (a second, lax axe net, say). `files`
 * stays the production subset, which is what a discipline guard wants.
 *
 * Vite's glob excludes the module that declares it, so this file is this
 * list's one blind spot; its consumers assert that.
 */
export const allFiles: [string, string][] = Object.entries(RAW);

/** Per-line scan: collect `path:line  excerpt` for every line matching `re`. */
export function scan(
  re: RegExp,
  allow: (path: string) => boolean = () => false,
): string[] {
  const hits: string[] = [];
  for (const [path, src] of files) {
    if (allow(path)) continue;
    src.split("\n").forEach((line, i) => {
      if (re.test(line)) hits.push(`${path}:${i + 1}  ${line.trim().slice(0, 100)}`);
    });
  }
  return hits;
}

/** Whole-file scan for patterns that span lines (e.g. a multiline JSX tag). */
export function scanWhole(
  re: RegExp,
  allow: (path: string) => boolean = () => false,
): string[] {
  const hits: string[] = [];
  for (const [path, src] of files) {
    if (allow(path)) continue;
    if (re.test(src)) hits.push(path);
  }
  return hits;
}
