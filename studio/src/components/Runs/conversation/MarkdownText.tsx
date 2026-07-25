import { memo } from "react";
import ReactMarkdown, { type Components, type Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";

interface Props {
  value: string;
  // Optional sizing by context: `sm` is compact node-card prose, `md` is the
  // regular run view, and `lg` is reserved for text the operator must read
  // before making a human-review decision.
  size?: "sm" | "md" | "lg";
}

// Module-scoped so the reference is stable across renders.
// `react-markdown` keys its internal parse cache on the components
// identity — a fresh object literal per render would invalidate it
// and reparse every tick. See:
// https://github.com/remarkjs/react-markdown#optimize
const COMPONENTS: Components = {
  h1: ({ node: _node, ...props }) => (
    <h3 className="font-semibold text-label mt-2 mb-1" {...props} />
  ),
  h2: ({ node: _node, ...props }) => (
    <h4 className="font-semibold text-body mt-2 mb-1" {...props} />
  ),
  h3: ({ node: _node, ...props }) => (
    <h5 className="font-semibold text-body mt-1 mb-0.5" {...props} />
  ),
  h4: ({ node: _node, ...props }) => (
    <h6
      className="font-medium text-micro uppercase tracking-wide text-fg-subtle mt-1 mb-0.5"
      {...props}
    />
  ),
  p: ({ node: _node, ...props }) => (
    <p className="my-1 whitespace-pre-wrap break-words" {...props} />
  ),
  ul: ({ node: _node, ...props }) => (
    <ul className="my-1 ml-4 list-disc space-y-0.5" {...props} />
  ),
  ol: ({ node: _node, ...props }) => (
    <ol className="my-1 ml-4 list-decimal space-y-0.5" {...props} />
  ),
  li: ({ node: _node, ...props }) => (
    <li className="leading-snug" {...props} />
  ),
  code: ({ node: _node, className, children, ...props }) => {
    // rehype-highlight tags every fenced (block) code element with the
    // base `hljs` class (plus `language-xxx` when known); inline code is
    // left untouched. So the presence of `hljs` is the reliable
    // block-vs-inline signal — checking `language-` alone would misclassify
    // auto-detected blocks (no fence language) as inline.
    const isInline = !className?.split(/\s+/).includes("hljs");
    if (isInline) {
      return (
        <code
          className="px-1 py-0.5 rounded bg-surface-2 text-micro font-mono"
          {...props}
        >
          {children}
        </code>
      );
    }
    return (
      <code className={className} {...props}>
        {children}
      </code>
    );
  },
  pre: ({ node: _node, ...props }) => (
    <pre
      className="my-2 px-2 py-1.5 rounded bg-surface-2 text-micro font-mono overflow-x-auto"
      {...props}
    />
  ),
  a: ({ node: _node, ...props }) => (
    <a
      className="text-accent-text underline underline-offset-2 hover:opacity-80"
      target="_blank"
      rel="noopener noreferrer"
      {...props}
    />
  ),
  blockquote: ({ node: _node, ...props }) => (
    <blockquote
      className="my-1 pl-2 border-l-2 border-border-subtle text-fg-muted"
      {...props}
    />
  ),
  table: ({ node: _node, ...props }) => (
    // Wide markdown tables would otherwise force the conversation
    // column to scroll horizontally; wrapping in an overflow container
    // keeps the table itself scrollable while the prose around it
    // stays contained.
    <div className="overflow-x-auto">
      <table className="my-2 border-collapse text-micro" {...props} />
    </div>
  ),
  th: ({ node: _node, ...props }) => (
    <th
      className="border border-border-subtle px-2 py-1 bg-surface-2 text-left font-medium"
      {...props}
    />
  ),
  td: ({ node: _node, ...props }) => (
    <td className="border border-border-subtle px-2 py-1" {...props} />
  ),
};

const REMARK_PLUGINS = [remarkGfm];
// rehype-highlight (highlight.js, synchronous) tags fenced code blocks
// with `hljs language-xxx` + `hljs-*` token spans. The colours are
// theme-mapped in app.css (`.prose-iterion .hljs-*` → `--hljs-*` tokens
// defined per [data-theme]). `detect: true` lets blocks without a fence
// language still get auto-highlighted; `ignoreMissing: true` keeps an
// unknown language from throwing — it just renders uncoloured.
const REHYPE_PLUGINS: Options["rehypePlugins"] = [
  [rehypeHighlight, { detect: true, ignoreMissing: true }],
];

// MarkdownText renders a markdown string with GFM extensions (tables,
// strikethrough, task lists, autolinks). The studio's `ui/MarkdownPreview`
// component is misnamed — its `preview` mode returns raw text, so we
// can't reuse it. react-markdown is small (~5KB gzip), MIT, and escapes
// HTML by default so untrusted agent output can't inject script tags.
// Memoised: the COMPONENTS/REMARK_PLUGINS objects are module-scoped and
// the props are a plain (value, size) pair, so the shallow compare is
// exact. Without it react-markdown re-parses every visible prompt/output
// card on each event tick of a streaming run.
function MarkdownText({ value, size = "md" }: Props) {
  const base =
    size === "sm"
      ? "text-micro"
      : size === "lg"
        ? "text-title leading-relaxed"
        : "text-body";
  return (
    <div
      className={`prose-iterion ${base} text-fg-default ${
        size === "lg" ? "" : "leading-snug"
      }`}
    >
      <ReactMarkdown
        remarkPlugins={REMARK_PLUGINS}
        rehypePlugins={REHYPE_PLUGINS}
        components={COMPONENTS}
      >
        {value}
      </ReactMarkdown>
    </div>
  );
}

export default memo(MarkdownText);
