// Map a file path to a Monaco language id. Used by FileDiffDialog so the
// DiffEditor lights up with syntax highlighting matching the file the
// user clicked. Unknown extensions fall through to "plaintext", which
// Monaco renders without errors.
//
// We only enumerate languages Monaco ships with by default — no
// dynamic-loaded grammars — so the diff dialog never has to wait on
// async language registration.
const EXT_TO_LANG: Record<string, string> = {
  ts: "typescript",
  tsx: "typescript",
  mts: "typescript",
  cts: "typescript",
  js: "javascript",
  jsx: "javascript",
  mjs: "javascript",
  cjs: "javascript",
  json: "json",
  go: "go",
  rs: "rust",
  py: "python",
  rb: "ruby",
  java: "java",
  kt: "kotlin",
  c: "c",
  h: "c",
  cpp: "cpp",
  hpp: "cpp",
  cc: "cpp",
  cs: "csharp",
  swift: "swift",
  php: "php",
  sh: "shell",
  bash: "shell",
  zsh: "shell",
  yml: "yaml",
  yaml: "yaml",
  toml: "ini",
  ini: "ini",
  md: "markdown",
  markdown: "markdown",
  html: "html",
  htm: "html",
  xml: "xml",
  css: "css",
  scss: "scss",
  less: "less",
  sql: "sql",
  dockerfile: "dockerfile",
};

const FILENAME_TO_LANG: Record<string, string> = {
  Dockerfile: "dockerfile",
  Makefile: "makefile",
  "go.mod": "go",
  "go.sum": "plaintext",
};

export function inferMonacoLanguage(path: string): string {
  if (!path) return "plaintext";
  const base = path.split("/").pop() ?? path;
  const named = FILENAME_TO_LANG[base];
  if (named) return named;
  // .iter is iterion's own DSL — not registered globally for this
  // dialog, so plaintext keeps Monaco from trying to tokenize.
  if (base.endsWith(".iter")) return "plaintext";
  const dot = base.lastIndexOf(".");
  if (dot < 0) return "plaintext";
  const ext = base.slice(dot + 1).toLowerCase();
  return EXT_TO_LANG[ext] ?? "plaintext";
}
