import { useDocumentStore } from "@/store/document";

export default function DiagnosticsPanel() {
  const diagnostics = useDocumentStore((s) => s.diagnostics);
  const warnings = useDocumentStore((s) => s.warnings);

  const hasIssues = diagnostics.length > 0 || warnings.length > 0;

  return (
    <div className="p-3 text-xs font-mono">
      <h2 className="font-bold text-gray-300 mb-1 text-sm font-sans">Diagnostics</h2>
      {!hasIssues && (
        <p className="text-gray-500">No issues.</p>
      )}
      {diagnostics.map((d, i) => (
        <div key={`e-${i}`} className="text-red-400 py-0.5">
          {d}
        </div>
      ))}
      {warnings.map((w, i) => (
        <div key={`w-${i}`} className="text-yellow-400 py-0.5">
          {w}
        </div>
      ))}
    </div>
  );
}
