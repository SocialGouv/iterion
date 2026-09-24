import { useEffect, useRef } from "react";
import { stampEditor, stampHolds, useDocumentStoreInstance } from "@/store/document";
import { changedExternally, offerReload } from "@/lib/reloadOffer";
import { useUIStore } from "@/store/ui";
import { fileWatcher } from "@/api/ws";
import * as api from "@/api/client";
import { errorMessage } from "@/lib/errorHints";
import { touchesOpenUnit } from "@/hooks/unitFiles";
import { applyOpenedFile } from "@/lib/openedFile";
import type { ServerWsEvent } from "@/api/types";

const RELOAD_DEBOUNCE_MS = 500;

// useFileWatcher subscribes the surrounding editor subtree to the
// server's file-changed stream. Multiple editor tabs each call this
// hook from their own EditorTabHost subtree, so each one only acts on
// events for the file it owns. The per-tab document store is captured
// via the Context-resolved instance — i.e. each watcher writes back
// to its own store, never to a sibling tab's.
export function useFileWatcher() {
  const reloadTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  // Capture the tab's document store. Stable across renders for one
  // tab mount; refreshed via useRef so the WS callback sees the same
  // identity even if Context shifts mid-flight.
  const docStore = useDocumentStoreInstance();
  const docStoreRef = useRef(docStore);
  docStoreRef.current = docStore;

  useEffect(() => {
    fileWatcher.connect();
    const deferred = new Set<() => void>();
    // An event that arrives while an explicit replacement is pending is
    // re-read once that replacement ends, against the tab as it left it:
    // a replacement that failed, was refused, or read the file before this
    // write leaves the tab on the file the event is about.
    const afterPending = (event: ServerWsEvent) => {
      const unsub = docStoreRef.current.subscribe((s) => {
        if (s._pendingIntent !== null) return;
        unsub();
        deferred.delete(unsub);
        handle(event);
      });
      deferred.add(unsub);
    };

    const onEvent = (event: ServerWsEvent) => {
      if (event.type !== "file_created" && event.type !== "file_modified" && event.type !== "file_deleted") {
        return;
      }
      if (event.type === "file_created" || event.type === "file_deleted") {
        useUIStore.getState().notifyFilesChanged();
      }
      handle(event);
    };

    const handle = (event: ServerWsEvent) => {
      const store = docStoreRef.current.getState();
      // An explicit replacement is in flight: the event waits for it to end
      // — an event about the very file being opened included, which the path
      // below does not name yet — and is then read against the tab as it
      // was left.
      if (store._pendingIntent !== null) {
        afterPending(event);
        return;
      }
      const { addToast } = useUIStore.getState();
      const filePath = store.currentFilePath;
      // An open Source-view edit counts, dirty or not: an auto-reload would
      // swap the document and the revision under what the author is typing,
      // and the Apply that follows would then land on top of whoever wrote
      // the file meanwhile. `hasUnsavedWork()` is the narrower question —
      // it does not hold for an editor opened and not yet typed in — so the
      // gate here stays the buffer's PRESENCE.
      const dirty = store.isDirty() || !!store.sourceBuffer;

      switch (event.type) {
        case "file_deleted":
          // The file itself, or — for a bot in several files — one of the
          // fragments its imports reach. The Source view's picker lists
          // them, and a fragment deleted from under it would otherwise
          // leave a name in the list that nothing answers for.
          if (!filePath || !touchesOpenUnit(event.path, filePath, store.unit)) break;
          addToast(
            event.path === filePath
              ? "Current file was deleted externally"
              : `${event.path} was deleted externally — it is one of this bot's files`,
            "warning",
            { persistent: true },
          );
          break;

        case "file_modified": {
          // The file itself, or — for a bot in several files — one of the
          // fragments its imports reach: the merged document and the
          // revision a save presents follow either.
          if (!filePath || !touchesOpenUnit(event.path, filePath, store.unit)) break;

          const name = (path: string) => path.split("/").pop() ?? path;

          // The AUTOMATIC reload: only while nothing of the author's could be
          // lost, read again at the answer — the round trip is a window in
          // which they can open a Source-view edit, and `applyOpenedFile`
          // drops the buffer. It stands down while an explicit replacement
          // is pending: that request speaks for the author, and a background
          // answer landing first would make its answer look refused for
          // edits nobody made. Through the shared helper: a reload of a file
          // that stopped parsing must mark the document a SALVAGE too, and
          // the revision a save presents follows the files on disk.
          const autoReload = (path: string) => {
            const asked = stampEditor(docStoreRef.current.getState());
            api
              .openFile(path)
              .then((result) => {
                const st = docStoreRef.current.getState();
                if (st.currentFilePath !== path) return;
                if (st._pendingIntent !== null) {
                  afterPending({ type: "file_modified", path: event.path } as ServerWsEvent);
                  return;
                }
                if (st.isDirty() || !!st.sourceBuffer) {
                  offerReload(docStoreRef.current, path, changedExternally(path));
                  return;
                }
                // The document moved while the file was read — an edit that
                // was saved since, another reload: what was read may be older
                // than what the tab shows, so the file is read again.
                if (!stampHolds(asked, st).generation) {
                  handle({ type: "file_modified", path: event.path } as ServerWsEvent);
                  return;
                }
                applyOpenedFile(result, st);
                useUIStore.getState().addToast("File reloaded", "info");
              })
              .catch((err) => {
                console.error("Failed to reload file:", err);
                // A silently dropped reload is worse than a sticky,
                // recoverable error that names the file.
                useUIStore.getState().addToast(`Failed to reload ${name(path)}: ${errorMessage(err)}`, "error", {
                  persistent: true,
                  action: { label: "Retry", onClick: () => autoReload(path) },
                });
              });
          };

          if (!dirty) {
            // Debounce auto-reload to avoid rapid re-parses. The path is
            // re-read inside the timer so a user switching the open file
            // between the event arrival and the 500ms fire doesn't get
            // the OLD file's contents stomped onto the new one.
            const targetPath = filePath;
            clearTimeout(reloadTimerRef.current);
            reloadTimerRef.current = setTimeout(() => {
              const current = docStoreRef.current.getState();
              if (current._pendingIntent !== null) {
                afterPending(event);
                return;
              }
              if (current.currentFilePath !== targetPath) return;
              // Work that appeared inside the window — an edit, or a
              // Source-view edit opened — keeps the reload from running, and
              // the change is offered instead of dropped.
              if (current.isDirty() || !!current.sourceBuffer) {
                offerReload(docStoreRef.current, targetPath, changedExternally(targetPath));
                return;
              }
              autoReload(targetPath);
            }, RELOAD_DEBOUNCE_MS);
          } else {
            // The commonest case: the edit was already open when the write
            // landed.
            offerReload(docStoreRef.current, filePath, changedExternally(filePath));
          }
          break;
        }
      }
    };
    const unsubscribe = fileWatcher.subscribe(onEvent);

    return () => {
      clearTimeout(reloadTimerRef.current);
      for (const u of deferred) u();
      unsubscribe();
      fileWatcher.disconnect();
    };
  }, []);
}
