import { Fragment } from "react";

// Matches bare http(s) URLs inside arbitrary text. Only these two schemes
// are recognised — a `javascript:` / `data:` payload in a run input must
// never become a clickable link. Trailing punctuation is trimmed after the
// match (see trimTrailing) so "see https://x/y." doesn't swallow the dot.
const URL_PATTERN = /https?:\/\/[^\s<>"']+/g;

// Closing punctuation that is almost always prose, not part of the URL.
// A trailing ")" is kept when the URL contains an unmatched "(" — wiki-style
// links like https://en.wikipedia.org/wiki/Foo_(bar) are common enough.
function trimTrailing(url: string): { url: string; rest: string } {
  let end = url.length;
  while (end > 0) {
    const ch = url[end - 1] ?? "";
    if (ch === ")") {
      const opens = (url.slice(0, end).match(/\(/g) ?? []).length;
      const closes = (url.slice(0, end).match(/\)/g) ?? []).length;
      if (opens >= closes) break;
    } else if (!".,;:!?'\"]}".includes(ch)) {
      break;
    }
    end -= 1;
  }
  return { url: url.slice(0, end), rest: url.slice(end) };
}

export interface LinkifiedTextProps {
  text: string;
  // Extra classes for the rendered anchors (defaults to the studio's
  // external-link styling).
  linkClassName?: string;
}

// LinkifiedText renders a plain string with its http(s) URLs turned into
// external links. Used wherever operator-supplied values are displayed
// verbatim (run inputs such as `pr_url`), where the value is data — never
// markup: the text is split and rendered as React nodes, never injected as
// HTML.
export function LinkifiedText({ text, linkClassName }: LinkifiedTextProps) {
  const parts: Array<string | { href: string }> = [];
  let cursor = 0;
  for (const m of text.matchAll(URL_PATTERN)) {
    const start = m.index ?? 0;
    const { url, rest } = trimTrailing(m[0]);
    if (!url) continue;
    if (start > cursor) parts.push(text.slice(cursor, start));
    parts.push({ href: url });
    if (rest) parts.push(rest);
    cursor = start + m[0].length;
  }
  if (parts.length === 0) return <>{text}</>;
  if (cursor < text.length) parts.push(text.slice(cursor));

  return (
    <>
      {parts.map((p, i) =>
        typeof p === "string" ? (
          <Fragment key={i}>{p}</Fragment>
        ) : (
          <a
            key={i}
            href={p.href}
            target="_blank"
            rel="noopener noreferrer"
            className={
              linkClassName ??
              "text-accent-text underline underline-offset-2 hover:opacity-80"
            }
          >
            {p.href}
          </a>
        ),
      )}
    </>
  );
}
