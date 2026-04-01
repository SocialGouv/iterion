import { useDocumentStore } from "@/store/document";

export default function DiagnosticsPanel() {
  const diagnostics = useDocumentStore((s) => s.diagnostics);
  const warnings = useDocumentStore((s) => s.warnings);

  const errorCount = diagnostics.length;
  const warningCount = warnings.length;
  const hasIssues = errorCount > 0 || warningCount > 0;

  return (
    <div className="p-3 text-xs font-mono h-full overflow-y-auto">
      <div className="flex items-center gap-3 mb-1">
        <h2 className="font-bold text-gray-300 text-sm font-sans">Diagnostics</h2>
        {hasIssues && (
          <div className="flex items-center gap-2 font-sans">
            {errorCount > 0 && (
              <span className="bg-red-900/50 text-red-400 px-1.5 py-0.5 rounded text-[10px]">
                {errorCount} error{errorCount !== 1 ? "s" : ""}
              </span>
            )}
            {warningCount > 0 && (
              <span className="bg-yellow-900/50 text-yellow-400 px-1.5 py-0.5 rounded text-[10px]">
                {warningCount} warning{warningCount !== 1 ? "s" : ""}
              </span>
            )}
          </div>
        )}
      </div>
      {!hasIssues && (
        <p className="text-green-500/70 font-sans">No issues found.</p>
      )}
      {diagnostics.map((d, i) => (
        <div key={`e-${i}`} className="text-red-400 py-0.5 flex items-start gap-1.5">
          <span className="text-red-600 shrink-0">&#x2716;</span>
          <span>{d}</span>
        </div>
      ))}
      {warnings.map((w, i) => (
        <div key={`w-${i}`} className="text-yellow-400 py-0.5 flex items-start gap-1.5">
          <span className="text-yellow-600 shrink-0">&#x26A0;</span>
          <span>{w}</span>
        </div>
      ))}
    </div>
  );
}
