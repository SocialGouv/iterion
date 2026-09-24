import * as api from "@/api/client";
import { errorMessage } from "@/lib/errorHints";
import { applyOpenedFile } from "@/lib/openedFile";
import { replaceDocument } from "@/lib/replaceDocument";
import type { DocumentState, DocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";

const storeIds = new WeakMap<DocumentStore, number>();
let lastStoreId = 0;
function storeId(store: DocumentStore): number {
  let id = storeIds.get(store);
  if (id === undefined) {
    id = ++lastStoreId;
    storeIds.set(store, id);
  }
  return id;
}

// What a reload would take from the author, said in its label: the canvas's
// unsaved edits and the Source view's un-applied text alike. A neutral
// "Reload" on a toast is one click away from the loss the automatic path's
// gate exists to prevent.
const reloadLabel = (st: DocumentState) => (st.hasUnsavedWork() ? "Reload and discard" : "Reload");

/** The offer raised when the file a tab shows was written by someone else. */
export const changedExternally = (path: string) => `${path} changed externally`;

/**
 * Offers a MANUAL reload of `path` into `store`, on a persistent toast. The
 * author asks for exactly this, so it skips the automatic gate — but only for
 * what the toast described when it was shown:
 * - clicked after the tab moved to another file, it does not reload that one;
 * - clicked under a label that no longer says what the click would take (work
 *   appeared, or went), it offers again with the accurate one;
 * - the reload goes through `replaceDocument`, so typing during its request
 *   refuses the answer rather than vanishing under it — and offers again,
 *   since what is on disk is still not on screen.
 *
 * One offer per tab and file: a newer one replaces it, and another tab's
 * offer — even about the same file — stands beside it.
 */
export function offerReload(store: DocumentStore, path: string, message: string): void {
  const name = path.split("/").pop() ?? path;
  const label = reloadLabel(store.getState());
  useUIStore.getState().addToast(message, "warning", {
    persistent: true,
    key: `reload:${storeId(store)}:${path}`,
    action: {
      label,
      onClick: () => {
        const now = store.getState();
        if (now.currentFilePath !== path) {
          useUIStore.getState().addToast(`${name} is no longer open in this tab, so it was not reloaded.`, "info");
          return;
        }
        if (reloadLabel(now) !== label) {
          offerReload(store, path, message);
          return;
        }
        replaceDocument(store, name, () => api.openFile(path), (result, st) => applyOpenedFile(result, st), {
          onRefused: () => offerReload(store, path, message),
        }).catch((err) => {
          console.error("Failed to reload file:", err);
          offerReload(store, path, `Failed to reload ${name}: ${errorMessage(err)}`);
        });
      },
    },
  });
}
