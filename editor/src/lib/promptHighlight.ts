/**
 * Lightweight tokenizer for prompt bodies — used by the textarea overlay
 * to syntax-highlight `{{...}}` template references and `${...}` env
 * variables without loading Monaco. Pure logic; covered by
 * `promptHighlight.test.ts`.
 *
 * The tokenizer mirrors the rules in `iterLanguage.ts` (the Monaco
 * grammar used by SourceView) but operates on plain strings and emits
 * a flat list of chunks. The overlay component is responsible for the
 * visual styling.
 */

export type HighlightKind = "text" | "ref" | "envvar" | "comment";

export interface HighlightChunk {
  kind: HighlightKind;
  text: string;
}

/**
 * Split a prompt body into a list of styled chunks. The concatenation
 * of `chunk.text` for every chunk is exactly the input — we never
 * lose, reorder, or rewrite characters. This is required so the
 * overlay layer aligns with the textarea below it.
 */
export function highlightPromptBody(src: string): HighlightChunk[] {
  if (!src) return [];

  const out: HighlightChunk[] = [];
  let buf = "";
  const flushText = () => {
    if (buf.length > 0) {
      out.push({ kind: "text", text: buf });
      buf = "";
    }
  };

  let i = 0;
  while (i < src.length) {
    const ch = src[i];

    // Line comments (## ... up to newline). Mirrors iterLanguage.ts.
    if (ch === "#" && src[i + 1] === "#") {
      flushText();
      const nl = src.indexOf("\n", i);
      const end = nl === -1 ? src.length : nl;
      out.push({ kind: "comment", text: src.slice(i, end) });
      i = end;
      continue;
    }

    // Template references {{ ... }}.
    if (ch === "{" && src[i + 1] === "{") {
      const close = src.indexOf("}}", i + 2);
      if (close !== -1) {
        flushText();
        const end = close + 2;
        out.push({ kind: "ref", text: src.slice(i, end) });
        i = end;
        continue;
      }
      // Unterminated `{{` — fall through and treat as text. The next
      // characters get consumed normally so we never spin.
    }

    // Env vars ${...}. Single-line, no nested braces.
    if (ch === "$" && src[i + 1] === "{") {
      const close = src.indexOf("}", i + 2);
      if (close !== -1) {
        flushText();
        const end = close + 1;
        out.push({ kind: "envvar", text: src.slice(i, end) });
        i = end;
        continue;
      }
    }

    buf += ch;
    i++;
  }

  flushText();
  return out;
}
