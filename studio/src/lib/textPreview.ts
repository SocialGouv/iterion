// Which inbound attachments a human gate can render inline as text.
// Images / audio / video stay on the media path; everything else that
// is a document or structured payload used to collapse to "Download".

export type TextPreviewKind = "json" | "markdown" | "text";

const JSON_EXTS = new Set(["json", "jsonl", "ndjson"]);
const MARKDOWN_EXTS = new Set(["md", "mdx", "markdown"]);
const TEXT_EXTS = new Set(["yml", "yaml", "txt", "csv", "tsv", "toml", "xml", "log"]);

function extension(filename: string): string {
  const base = filename.slice(Math.max(filename.lastIndexOf("/"), filename.lastIndexOf("\\")) + 1);
  const dot = base.lastIndexOf(".");
  if (dot <= 0) return "";
  return base.slice(dot + 1).toLowerCase();
}

function mimeType(mime: string): string {
  const [type] = mime.toLowerCase().split(";");
  return (type ?? "").trim();
}

/** How to render an attachment, or null when it is not inline-text. */
export function textPreviewKind(mime: string, filename = ""): TextPreviewKind | null {
  const m = mimeType(mime);
  const ext = extension(filename);

  if (m === "application/json" || JSON_EXTS.has(ext)) return "json";
  if (m === "text/markdown" || m === "text/x-markdown" || MARKDOWN_EXTS.has(ext)) {
    return "markdown";
  }
  if (
    m.startsWith("text/") ||
    m === "application/yaml" ||
    m === "application/x-yaml" ||
    m === "application/xml" ||
    m === "application/toml" ||
    TEXT_EXTS.has(ext)
  ) {
    return "text";
  }
  return null;
}

/** Pretty-print JSON when the body is valid; otherwise leave it raw. */
export function prettyJSON(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}
