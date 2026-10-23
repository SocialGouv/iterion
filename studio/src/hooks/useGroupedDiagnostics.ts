import { useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { groupDiagnostics, withReadRefusalHint, type GroupedDiagnostics } from "@/lib/diagnostics";

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
  const readRefusal = useDocumentStore((s) => s.readRefusal);

  return useMemo(
    () => withReadRefusalHint(groupDiagnostics(diagnostics, warnings, document, issues), readRefusal),
    [diagnostics, warnings, document, issues, readRefusal],
  );
}
