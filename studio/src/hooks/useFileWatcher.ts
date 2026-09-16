import { useEffect, useRef } from "react";
import { useDocumentStoreInstance } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { fileWatcher } from "@/api/ws";
import * as api from "@/api/client";
import { errorMessage } from "@/lib/errorHints";
import { touchesOpenUnit } from "@/hooks/unitFiles";
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
      const dirty = store.isDirty();

      if (event.type === "file_created" || event.type === "file_deleted") {
        notifyFilesChanged();
      }

      switch (event.type) {
        case "file_deleted":
          if (event.path === filePath) {
            addToast("Current file was deleted externally", "warning", { persistent: true });
          }
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
                s.setDocument(result.document);
                s.setDiagnostics(result.diagnostics);
                // The files changed on disk: the revision a save presents must follow.
                s.setUnit(result.unit ?? null);
                s.setCurrentSource(result.source);
                s.markSaved();
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
              if (current.currentFilePath !== targetPath || current.isDirty()) {
                return;
              }
              reload(targetPath, true);
            }, RELOAD_DEBOUNCE_MS);
          } else {
            addToast("File changed externally", "warning", {
              persistent: true,
              action: {
                label: "Reload",
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
