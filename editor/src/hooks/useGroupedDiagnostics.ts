import { useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { groupDiagnostics, type GroupedDiagnostics } from "@/lib/diagnostics";

/**
 * Memoized grouping of the current document's diagnostics + warnings.
 * Components consume this to render inline badges on nodes/edges and to
 * filter the bottom diagnostics panel.
 */
export function useGroupedDiagnostics(): GroupedDiagnostics {
  const document = useDocumentStore((s) => s.document);
  const diagnostics = useDocumentStore((s) => s.diagnostics);
  const warnings = useDocumentStore((s) => s.warnings);
  const issues = useDocumentStore((s) => s.issues);

  return useMemo(
    () => groupDiagnostics(diagnostics, warnings, document, issues),
    [diagnostics, warnings, document, issues],
  );
}
