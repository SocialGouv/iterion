import { useEffect, useRef } from "react";
import { useDocumentStore } from "@/store/document";
import * as api from "@/api/client";

export function useAutoValidation() {
  const document = useDocumentStore((s) => s.document);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    if (!document) return;
    clearTimeout(timerRef.current);
    timerRef.current = setTimeout(async () => {
      try {
        const result = await api.validate(document);
        setDiagnostics(result.diagnostics, result.warnings);
      } catch {
        // silently ignore validation errors during auto-validation
      }
    }, 1500);
    return () => clearTimeout(timerRef.current);
  }, [document, setDiagnostics]);
}
