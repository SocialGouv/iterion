import { useEffect, useRef } from "react";
import { useDocumentStore } from "@/store/document";
import * as api from "@/api/client";

export function useAutoValidation() {
  const document = useDocumentStore((s) => s.document);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
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
      try {
        const result = await api.validate(document, controller.signal);
        if (!controller.signal.aborted) {
          setDiagnostics(result.diagnostics, result.warnings, result.issues);
        }
      } catch {
        // silently ignore validation errors during auto-validation
      }
    }, 1500);
    return () => clearTimeout(timerRef.current);
  }, [document, setDiagnostics]);
}
