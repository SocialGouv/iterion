import { useEffect, useRef } from "react";
import { stampEditor, stampHolds, useDocumentStore, useDocumentStoreInstance } from "@/store/document";
import * as api from "@/api/client";

export function useAutoValidation() {
  const document = useDocumentStore((s) => s.document);
  // The file the document came from: a main.bot inside a bundle is
  // validated with the bundle's prompts/*.md in scope (see api.validate).
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const docStore = useDocumentStoreInstance();
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  const abortRef = useRef<AbortController>(undefined);

  useEffect(() => {
    if (!document) return;
    clearTimeout(timerRef.current);
    timerRef.current = setTimeout(async () => {
      // Abort any in-flight validation to prevent stale results
      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;
      // The diagnostics describe THIS document of THIS file. An answer that
      // lands after either moved is about something no longer on screen —
      // the next run validates what is there now.
      const asked = stampEditor(docStore.getState());
      try {
        const result = await api.validate(document, controller.signal, currentFilePath);
        if (controller.signal.aborted) return;
        const holds = stampHolds(asked, docStore.getState());
        if (!holds.path || !holds.generation) return;
        setDiagnostics(result.diagnostics, result.warnings, result.issues);
      } catch (err) {
        // AbortError is expected when a newer keystroke supersedes
        // this request; everything else (network failure, 5xx, parse
        // error) means stale diagnostics are now misleading users.
        // Surface it to the console so devs notice but keep the UI
        // quiet — a toast on every transient hiccup during typing
        // would be worse than no signal at all.
        if (err instanceof Error && err.name === "AbortError") return;
        console.warn("[useAutoValidation] validate failed:", err);
      }
    }, 1500);
    return () => {
      clearTimeout(timerRef.current);
      // A request already in flight is for the document this effect was
      // scheduled with; once that moves, or the editor goes away, its answer
      // has nothing left to describe.
      abortRef.current?.abort();
    };
  }, [document, currentFilePath, setDiagnostics, docStore]);
}
