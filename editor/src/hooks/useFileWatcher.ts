import { useEffect, useRef } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { fileWatcher } from "@/api/ws";
import * as api from "@/api/client";
import type { FileEvent } from "@/api/types";

const RELOAD_DEBOUNCE_MS = 500;

export function useFileWatcher() {
  const reloadTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    fileWatcher.connect();

    const unsubscribe = fileWatcher.subscribe((event: FileEvent) => {
      const { currentFilePath, isDirty } = useDocumentStore.getState();
      const { addToast, notifyFilesChanged } = useUIStore.getState();
      const filePath = currentFilePath;
      const dirty = isDirty();

      if (event.type === "file_created" || event.type === "file_deleted") {
        notifyFilesChanged();
      }

      switch (event.type) {
        case "file_deleted":
          if (event.path === filePath) {
            addToast("Current file was deleted externally", "warning", { persistent: true });
          }
          break;

        case "file_modified":
          if (event.path !== filePath) break;
          if (!dirty) {
            // Debounce auto-reload to avoid rapid re-parses.
            clearTimeout(reloadTimerRef.current);
            reloadTimerRef.current = setTimeout(() => {
              api.openFile(event.path).then((result) => {
                const store = useDocumentStore.getState();
                store.setDocument(result.document);
                store.setDiagnostics(result.diagnostics);
                store.setCurrentSource(result.source);
                store.markSaved();
                useUIStore.getState().addToast("File reloaded", "info");
              }).catch((err) => {
                console.error("Failed to reload file:", err);
                useUIStore.getState().addToast("Failed to reload file", "error");
              });
            }, RELOAD_DEBOUNCE_MS);
          } else {
            addToast("File changed externally", "warning", {
              persistent: true,
              action: {
                label: "Reload",
                onClick: () => {
                  const path = useDocumentStore.getState().currentFilePath;
                  if (!path) return;
                  api.openFile(path).then((result) => {
                    const store = useDocumentStore.getState();
                    store.setDocument(result.document);
                    store.setDiagnostics(result.diagnostics);
                    store.setCurrentSource(result.source);
                    store.markSaved();
                  }).catch((err) => {
                    console.error("Failed to reload file:", err);
                    useUIStore.getState().addToast("Failed to reload file", "error");
                  });
                },
              },
            });
          }
          break;
      }
    });

    return () => {
      clearTimeout(reloadTimerRef.current);
      unsubscribe();
      fileWatcher.disconnect();
    };
  }, []);
}
