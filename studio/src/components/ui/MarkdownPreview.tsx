import { useMemo, useState, type ReactNode } from "react";

interface Props {
  value: string;
  onChange: (next: string) => void;
  /** Optional className applied to the editor textarea. */
  editorClassName?: string;
  rows?: number;
  placeholder?: string;
}

/** MarkdownPreview is a small Edit / Preview toggle for body text.
 *  No external markdown library is wired in this iteration — preview
 *  shows the raw text in a monospace block so the toggle exists as
 *  a UX scaffold. Wire react-markdown later (or remark) when the host
 *  is willing to pull the dep.
 */
export function MarkdownPreview({
  value,
  onChange,
  editorClassName = "",
  rows = 6,
  placeholder,
}: Props) {
  const [mode, setMode] = useState<"edit" | "preview">("edit");
  const preview = useMemo<ReactNode>(() => {
    if (!value.trim()) {
      return (
        <span className="text-fg-subtle italic">Nothing to preview yet.</span>
      );
    }
    return value;
  }, [value]);

  return (
    <div className="space-y-1">
      <div className="flex items-center gap-1 text-[10px]">
        <button
          type="button"
          onClick={() => setMode("edit")}
          className={`px-1.5 py-0.5 rounded ${
            mode === "edit"
              ? "bg-surface-2 text-fg-default"
              : "text-fg-subtle hover:text-fg-default"
          }`}
        >
          Edit
        </button>
        <button
          type="button"
          onClick={() => setMode("preview")}
          className={`px-1.5 py-0.5 rounded ${
            mode === "preview"
              ? "bg-surface-2 text-fg-default"
              : "text-fg-subtle hover:text-fg-default"
          }`}
        >
          Preview
        </button>
      </div>
      {mode === "edit" ? (
        <textarea
          value={value}
          rows={rows}
          placeholder={placeholder}
          onChange={(e) => onChange(e.target.value)}
          className={`w-full bg-surface-1 text-fg-default rounded-md border border-border-strong outline-none focus:border-accent focus:ring-1 focus:ring-accent px-2 py-1.5 text-xs font-mono resize-y ${editorClassName}`.trim()}
        />
      ) : (
        <div className="w-full min-h-[6rem] bg-surface-1 text-fg-default rounded-md border border-border-default px-2 py-1.5 text-xs font-mono whitespace-pre-wrap">
          {preview}
        </div>
      )}
    </div>
  );
}
