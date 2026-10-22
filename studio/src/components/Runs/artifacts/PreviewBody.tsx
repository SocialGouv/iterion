// PreviewBody renders one artifact file inside the files panel's preview
// dialog. Text bodies show as source; a markdown body renders through the
// conversation's MarkdownText (same GFM + highlight pipeline as assistant
// messages) with a toggle to the source — a report is read, not decoded.
// Images render inline; anything else points at Download.
import { useState } from "react";
import MarkdownText from "../conversation/MarkdownText";
import type { PreviewState } from "../usePreview";
import { isMarkdownType } from "./artifactFileLink";

export default function PreviewBody({ preview }: { preview: PreviewState }) {
  const [showSource, setShowSource] = useState(false);
  if (preview.loading) {
    return (
      <div className="h-64 flex items-center justify-center text-fg-subtle text-xs">
        Loading preview…
      </div>
    );
  }
  if (preview.error) {
    return <div className="text-xs text-danger">Failed to load: {preview.error}</div>;
  }
  if (preview.textBody !== null) {
    const markdown = isMarkdownType(preview.contentType);
    const source = (
      <pre className="max-h-[70vh] overflow-auto text-xs font-mono whitespace-pre-wrap break-words bg-surface-0 p-3 rounded">
        {preview.textBody}
      </pre>
    );
    if (!markdown) return source;
    // A toggle button keeps one label and carries its state in
    // aria-pressed (WAI-ARIA APG): "Show source, pressed" is the source
    // view, "not pressed" the rendered one. A label that flipped with the
    // state would announce the opposite of what is on screen.
    return (
      <div className="flex flex-col gap-2">
        <div className="flex justify-end">
          <button
            type="button"
            className="text-micro text-accent-text hover:underline"
            aria-pressed={showSource}
            onClick={() => setShowSource((v) => !v)}
          >
            Show source
          </button>
        </div>
        {showSource ? (
          source
        ) : (
          <div className="max-h-[70vh] overflow-auto bg-surface-0 p-4 rounded">
            <MarkdownText value={preview.textBody} />
          </div>
        )}
      </div>
    );
  }
  if (preview.blobURL && preview.contentType.startsWith("image/")) {
    return (
      <div className="max-h-[70vh] overflow-auto flex items-center justify-center bg-surface-0 p-3 rounded">
        <img src={preview.blobURL} alt={preview.path} className="max-w-full" />
      </div>
    );
  }
  return (
    <div className="text-xs text-fg-subtle py-6 text-center">
      Preview not available for this file type. Use Download to save it.
    </div>
  );
}
