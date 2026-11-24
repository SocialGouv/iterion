import type { IterDocument } from "@/api/types";

/** What the Source view knows about the text on screen: whether it is a
 *  SALVAGE — the file's own text rather than a render of the document — and
 *  which document the last completed render came from. */
export interface SourceProvenance {
  salvaged: boolean;
  document: IterDocument | null;
  /** The view's last completed render. Its `key` is the FILE axis, which is
   *  not read here; `doc` is the document that render was produced from. */
  rendered: { doc: IterDocument | null } | null;
}

/**
 * The document an Apply of the text on screen presents as the one that text
 * was written against.
 *
 * One expression, two readers: `handleApply`'s `moved()` compares it to the
 * store's current document and refuses the write when they differ, and the
 * header's badge reports that same comparison. Computed in one place so the
 * label and the refusal cannot disagree — a badge announcing a refusal that
 * no longer fires, or a refusal nothing announced, are both worse than
 * either alone.
 *
 * While salvaged the text is the FILE's and not a render, so no render's
 * provenance speaks for it: replacing the canvas with it is governed by its
 * own confirm instead. Before the first render lands there is no provenance
 * at all, and an Apply is written against the document as it is.
 *
 * What this is NOT, so the next reader does not route it there: the `doc` a
 * published `SourceBuffer` carries is the document its TEXT came from — the
 * render's, salvage or not — because a remount adopts it back as the
 * provenance to refuse against, and the salvage term here would hand that
 * adoption a document no render produced.
 */
export function appliedFromDocument({
  salvaged,
  document,
  rendered,
}: SourceProvenance): IterDocument | null {
  return salvaged ? document : (rendered?.doc ?? document);
}
