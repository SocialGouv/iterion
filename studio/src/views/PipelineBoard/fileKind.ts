// Classification of a produced pipeline file into a coarse media kind, used
// to pick an icon + preview strategy in the pipeline board sidebar's
// "Produced elements" section. A pipeline can emit code, images, music,
// video, documents, structured data, or archives; the kind drives both the
// row icon and whether the preview modal renders an <img>/<audio>/<video>.

export type ProducedFileKind =
  | "code"
  | "image"
  | "audio"
  | "video"
  | "doc"
  | "data"
  | "archive"
  | "other";

// EXT_KIND maps a lowercase extension (no dot) to its kind. Kept as a flat
// map (not per-kind arrays) so lookup is a single O(1) hit and adding a new
// extension is a one-line edit. Extensions absent here fall back to "other".
const EXT_KIND: Record<string, ProducedFileKind> = {
  // source code
  go: "code", ts: "code", tsx: "code", js: "code", jsx: "code", mjs: "code",
  cjs: "code", py: "code", rb: "code", rs: "code", java: "code", kt: "code",
  c: "code", h: "code", cc: "code", cpp: "code", hpp: "code", cs: "code",
  php: "code", swift: "code", scala: "code", sh: "code", bash: "code",
  zsh: "code", ps1: "code", sql: "code", html: "code", htm: "code",
  css: "code", scss: "code", sass: "code", less: "code", vue: "code",
  svelte: "code", lua: "code", dart: "code", ex: "code", exs: "code",
  bot: "code",
  // images
  png: "image", jpg: "image", jpeg: "image", gif: "image", webp: "image",
  svg: "image", avif: "image", bmp: "image", ico: "image", tif: "image",
  tiff: "image", heic: "image",
  // audio
  mp3: "audio", wav: "audio", ogg: "audio", oga: "audio", flac: "audio",
  m4a: "audio", aac: "audio", opus: "audio", weba: "audio", mid: "audio",
  midi: "audio",
  // video
  mp4: "video", webm: "video", mov: "video", mkv: "video", avi: "video",
  m4v: "video", wmv: "video", flv: "video",
  // documents / prose
  md: "doc", mdx: "doc", markdown: "doc", txt: "doc", pdf: "doc",
  doc: "doc", docx: "doc", rtf: "doc", odt: "doc", tex: "doc",
  // structured data / config
  json: "data", yaml: "data", yml: "data", toml: "data", xml: "data",
  csv: "data", tsv: "data", ini: "data", env: "data", parquet: "data",
  ndjson: "data", jsonl: "data",
  // archives / binaries
  zip: "archive", tar: "archive", gz: "archive", tgz: "archive",
  bz2: "archive", xz: "archive", "7z": "archive", rar: "archive",
  jar: "archive", war: "archive", deb: "archive", rpm: "archive",
};

// classifyProducedFile returns the media kind for a slash-separated path.
// Case-insensitive; a path with no extension (a Makefile, a bare LICENSE)
// resolves to "other".
export function classifyProducedFile(path: string): ProducedFileKind {
  const base = path.slice(path.lastIndexOf("/") + 1);
  const dot = base.lastIndexOf(".");
  // Leading-dot dotfiles (".gitignore") have no real extension.
  if (dot <= 0) return "other";
  const ext = base.slice(dot + 1).toLowerCase();
  return EXT_KIND[ext] ?? "other";
}

// producedKindLabel is the human word for a kind, used in aria labels and the
// per-row tooltip.
export function producedKindLabel(kind: ProducedFileKind): string {
  switch (kind) {
    case "code":
      return "Code";
    case "image":
      return "Image";
    case "audio":
      return "Audio";
    case "video":
      return "Video";
    case "doc":
      return "Document";
    case "data":
      return "Data";
    case "archive":
      return "Archive";
    default:
      return "File";
  }
}
