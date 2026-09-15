/** ProvenanceChip names the file a declaration came from, in a document of
 *  a bot in several files (`import "lib/x.bot"`). Read-only by design: the
 *  save writes each declaration back to its file and a new one to the
 *  main; moving a declaration between files is done in the files. */
export default function ProvenanceChip({ file }: { file: string }) {
  return (
    <div className="px-3 py-1 border-b border-border-default shrink-0">
      <span
        className="inline-flex items-center gap-1 rounded-full bg-surface-2 px-2 py-0.5 text-micro text-fg-subtle"
        title="Declared in this file of the bot. Edits save back to it; the editor does not move a declaration between files."
        data-testid="provenance-chip"
      >
        <span aria-hidden>📄</span>
        <span className="font-mono">{file}</span>
      </span>
    </div>
  );
}
