import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

interface Props {
  value: string;
  // Optional: shrink the prose for inline node-output cards. Default
  // keeps the standard 12px body size used elsewhere in the run view.
  size?: "sm" | "md";
}

// Module-scoped so the reference is stable across renders.
// `react-markdown` keys its internal parse cache on the components
// identity — a fresh object literal per render would invalidate it
// and reparse every tick. See:
// https://github.com/remarkjs/react-markdown#optimize
const COMPONENTS: Components = {
  h1: ({ node: _node, ...props }) => (
    <h3 className="font-semibold text-[13px] mt-2 mb-1" {...props} />
  ),
  h2: ({ node: _node, ...props }) => (
    <h4 className="font-semibold text-[12px] mt-2 mb-1" {...props} />
  ),
  h3: ({ node: _node, ...props }) => (
    <h5 className="font-semibold text-[12px] mt-1 mb-0.5" {...props} />
  ),
  h4: ({ node: _node, ...props }) => (
    <h6
      className="font-medium text-[11px] uppercase tracking-wide text-fg-subtle mt-1 mb-0.5"
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
    const isInline = !className?.startsWith("language-");
    if (isInline) {
      return (
        <code
          className="px-1 py-0.5 rounded bg-surface-2 text-[11px] font-mono"
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
      className="my-2 px-2 py-1.5 rounded bg-surface-2 text-[11px] font-mono overflow-x-auto"
      {...props}
    />
  ),
  a: ({ node: _node, ...props }) => (
    <a
      className="text-accent underline underline-offset-2 hover:opacity-80"
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
    <table className="my-2 border-collapse text-[11px]" {...props} />
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

// MarkdownText renders a markdown string with GFM extensions (tables,
// strikethrough, task lists, autolinks). The studio's `ui/MarkdownPreview`
// component is misnamed — its `preview` mode returns raw text, so we
// can't reuse it. react-markdown is small (~5KB gzip), MIT, and escapes
// HTML by default so untrusted agent output can't inject script tags.
export default function MarkdownText({ value, size = "md" }: Props) {
  const base = size === "sm" ? "text-[11px]" : "text-[12px]";
  return (
    <div className={`prose-iterion ${base} text-fg-default leading-snug`}>
      <ReactMarkdown remarkPlugins={REMARK_PLUGINS} components={COMPONENTS}>
        {value}
      </ReactMarkdown>
    </div>
  );
}
