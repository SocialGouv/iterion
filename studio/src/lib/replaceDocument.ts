import { stampEditor, stampHolds, type DocumentState, type DocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";

export type ReplaceOutcome = "applied" | "superseded" | "refused";

/**
 * How long a replacement waits for its answer. While it waits, the tab's
 * background writers (the watcher's reload, the assistant, a draft) stand
 * down and Save As is refused: a request the server accepted and never
 * answered would hold all of them down for the tab's lifetime. Past it, the
 * request is aborted, nothing is replaced, and the caller is told why.
 */
export const REPLACE_DEADLINE_MS = 60_000;

/** A replacement's load that did not answer within `REPLACE_DEADLINE_MS`.
 *  Typed, so the surfaces that read an error's message for a diagnosis
 *  (a missing file, a known kind of failure) leave it alone: the label in it
 *  is a name, which may contain anything. */
export class ReplaceDeadlineError extends Error {
  constructor(label: string) {
    super(`The server did not answer the request for ${label} within ${REPLACE_DEADLINE_MS / 1000} s; nothing was replaced.`);
    this.name = "ReplaceDeadlineError";
  }
}

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
 * A load that fails throws to the caller, which owns its error surface — and
 * so does one that does not answer within `REPLACE_DEADLINE_MS`: its request
 * is aborted through the signal `load` receives, and an answer arriving later
 * is never applied. A request a newer one superseded is the exception: its
 * failure, like its answer, is dropped quietly. A caller may own the
 * refusal's surface too (`onRefused`, in place of the toast).
 */
export async function replaceDocument<T>(
  store: DocumentStore,
  label: string,
  load: (signal: AbortSignal) => Promise<T>,
  apply: (answer: T, state: DocumentState) => void,
  options?: { onRefused?: () => void },
): Promise<ReplaceOutcome> {
  const intent = store.getState().beginReplace();
  const asked = stampEditor(store.getState());
  const request = new AbortController();
  let deadline: ReturnType<typeof setTimeout> | undefined;
  try {
    const answer = await Promise.race([
      load(request.signal),
      new Promise<never>((_, reject) => {
        deadline = setTimeout(() => {
          request.abort();
          reject(new ReplaceDeadlineError(label));
        }, REPLACE_DEADLINE_MS);
      }),
    ]);
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
  } catch (err) {
    if (!stampHolds(asked, store.getState()).intent) return "superseded";
    throw err;
  } finally {
    clearTimeout(deadline);
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
