/**
 * Registry of editable buffers that live OUTSIDE any document store.
 *
 * `hasUnsavedWork()` answers for a tab's document and its Source view, and
 * every discard path scoped to a tab consults it. Two buffers answer to
 * neither: the bundle-files drawer's per-file buffer and the run file-edit
 * dialog's worktree buffer. Both are keyed by what they edit (team/slug,
 * run/path) rather than held by a tab, so the question they raise is
 * app-wide — the browser-unload warning above all, which is the one surface
 * that fires while their owner is not even mounted.
 *
 * Holders register a dirtiness probe and un-register on release; the answer
 * is read at ask time, never cached.
 */
const holders = new Set<() => boolean>();

/** Registers a dirtiness probe; returns its un-registration. */
export function registerUnsavedBuffer(isDirty: () => boolean): () => void {
  holders.add(isDirty);
  return () => {
    holders.delete(isDirty);
  };
}

/** Whether any registered buffer holds un-applied text. */
export function anyUnsavedBuffer(): boolean {
  for (const isDirty of holders) {
    if (isDirty()) return true;
  }
  return false;
}
