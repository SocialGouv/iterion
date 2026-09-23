import { useEffect, useRef } from "react";
import { useDocumentStoreInstance } from "@/store/document";
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

    const unsubscribe = fileWatcher.subscribe((event: ServerWsEvent) => {
      if (event.type !== "file_created" && event.type !== "file_modified" && event.type !== "file_deleted") {
        return;
      }
      const store = docStoreRef.current.getState();
      const { addToast, notifyFilesChanged } = useUIStore.getState();
      const filePath = store.currentFilePath;
      // An open Source-view edit counts, dirty or not: an auto-reload would
      // swap the document and the revision under what the author is typing,
      // and the Apply that follows would then land on top of whoever wrote
      // the file meanwhile. `hasUnsavedWork()` is the narrower question —
      // it does not hold for an editor opened and not yet typed in — so the
      // gate here stays the buffer's PRESENCE.
      const dirty = store.isDirty() || !!store.sourceBuffer;

      if (event.type === "file_created" || event.type === "file_deleted") {
        notifyFilesChanged();
      }

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

          // Shared reload: re-open the file and apply it to this tab's
          // store. On failure, surface an actionable toast that names the
          // file and offers a Retry — a silently dropped reload is worse
          // than a sticky, recoverable error. `notifySuccess` is set only
          // for the automatic (clean-buffer) path; the manual Reload
          // action stays quiet on success.
          const reload = (path: string, notifySuccess: boolean) => {
            api
              .openFile(path)
              .then((result) => {
                const s = docStoreRef.current.getState();
                if (s.currentFilePath !== path) return;
                // The gate is read once more HERE, next to the write. It was
                // read at the event and again at the debounce, but the round
                // trip is a third window: the author can open a Source-view
                // edit while `openFile` is in flight, and applying the answer
                // would swap the document and the revision under what they
                // are typing — `setCurrentFilePath` inside `applyOpenedFile`
                // then drops the buffer, so the text goes invisible to every
                // discard path AND their next Apply is refused as stale.
                // Only the automatic path is gated: the manual Reload action
                // is the author asking for exactly this.
                if (notifySuccess && (s.isDirty() || !!s.sourceBuffer)) {
                  // The action takes whatever is unsaved, and the manual path
                  // deliberately skips the gate — so the LABEL has to say so.
                  // A neutral "Reload" on a toast is one click away from the
                  // loss this guard exists to prevent.
                  useUIStore.getState().addToast("File changed externally", "warning", {
                    persistent: true,
                    action: {
                      label: s.isSourceDirty() ? "Reload and discard" : "Reload",
                      onClick: () => reload(path, false),
                    },
                  });
                  return;
                }
                // Through the shared helper: a reload of a file that stopped
                // parsing must mark the document a SALVAGE too. An external
                // write is all it takes, with no user action, and the next
                // save would put the salvage on the author's file.
                // The files changed on disk: the revision a save presents must follow.
                applyOpenedFile(result, s);
                if (notifySuccess) {
                  useUIStore.getState().addToast("File reloaded", "info");
                }
              })
              .catch((err) => {
                console.error("Failed to reload file:", err);
                const name = path.split("/").pop() ?? path;
                useUIStore.getState().addToast(
                  `Failed to reload ${name}: ${errorMessage(err)}`,
                  "error",
                  {
                    persistent: true,
                    action: {
                      label: "Retry",
                      onClick: () => reload(path, notifySuccess),
                    },
                  },
                );
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
              // The buffer too: the author can open a Source-view edit
              // inside this debounce window, and reading isDirty() alone
              // here loses the guard the branch above applies.
              if (
                current.currentFilePath !== targetPath ||
                current.isDirty() ||
                !!current.sourceBuffer
              ) {
                return;
              }
              reload(targetPath, true);
            }, RELOAD_DEBOUNCE_MS);
          } else {
            // The commonest case: the edit was already open when the write
            // landed. The action applies the reload without a second prompt,
            // so the label says what it takes — the same sentence the other
            // window owes, and for the same reason.
            addToast("File changed externally", "warning", {
              persistent: true,
              action: {
                label: store.isSourceDirty() ? "Reload and discard" : "Reload",
                onClick: () => {
                  const path = docStoreRef.current.getState().currentFilePath;
                  if (path) reload(path, false);
                },
              },
            });
          }
          break;
        }
      }
    });

    return () => {
      clearTimeout(reloadTimerRef.current);
      unsubscribe();
      fileWatcher.disconnect();
    };
  }, []);
}
