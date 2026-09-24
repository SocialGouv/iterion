import { stampEditor, stampHolds, type DocumentState, type DocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";

export type ReplaceOutcome = "applied" | "superseded" | "refused";

/**
 * Replaces a tab's document with the answer of an async load. The one path
 * every EXPLICIT replacement takes once its confirm has been answered:
 * opening a file or an example, an import, a manual reload.
 *
 * The confirm asked about the work present when it was clicked. The answer
 * lands later, and by then:
 * - a newer replacement may have been asked for: this answer is dropped
 *   quietly, because the newer request speaks for the author;
 * - the author may have edited, either the document or the Source view's
 *   text (which moves no generation): the answer is refused, and says so,
 *   because applying it would destroy work nobody was asked about.
 *
 * A load that fails throws to the caller, which owns its error surface. A
 * caller may own the refusal's too (`onRefused`, in place of the toast).
 */
export async function replaceDocument<T>(
  store: DocumentStore,
  label: string,
  load: () => Promise<T>,
  apply: (answer: T, state: DocumentState) => void,
  options?: { onRefused?: () => void },
): Promise<ReplaceOutcome> {
  const intent = store.getState().beginReplace();
  const asked = stampEditor(store.getState());
  try {
    const answer = await load();
    const now = store.getState();
    const holds = stampHolds(asked, now);
    if (!holds.intent) return "superseded";
    if (!holds.generation || !holds.source) {
      if (options?.onRefused) options.onRefused();
      else
        useUIStore.getState().addToast(
          `The editor changed while ${label} was opening, so it was not opened. Open it again to replace what is there now.`,
          "warning",
          { persistent: true },
        );
      return "refused";
    }
    now.markReplaced();
    apply(answer, store.getState());
    return "applied";
  } finally {
    store.getState().endReplace(intent);
  }
}

/**
 * A replacement with no answer to wait for (File → New, Start blank): it
 * still counts as the author's latest request, so an Open still in flight —
 * asked for BEFORE it — does not land on what it put there.
 */
export function replaceDocumentNow(store: DocumentStore, apply: (state: DocumentState) => void): void {
  const intent = store.getState().beginReplace();
  try {
    store.getState().markReplaced();
    apply(store.getState());
  } finally {
    store.getState().endReplace(intent);
  }
}
