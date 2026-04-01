import { useCallback, useEffect, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { createEmptyDocument } from "@/lib/defaults";
import * as api from "@/api/client";

export default function Toolbar() {
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const document = useDocumentStore((s) => s.document);
  const sourceViewOpen = useUIStore((s) => s.sourceViewOpen);
  const toggleSourceView = useUIStore((s) => s.toggleSourceView);

  const [examples, setExamples] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    api.listExamples().then(setExamples).catch(console.error);
  }, []);

  const handleNew = useCallback(() => {
    setDocument(createEmptyDocument());
    setDiagnostics([], []);
  }, [setDocument, setDiagnostics]);

  const loadExample = useCallback(
    async (name: string) => {
      if (!name) return;
      setLoading(true);
      try {
        const result = await api.loadExample(name);
        setDocument(result.document);
        setDiagnostics(result.diagnostics);
      } catch (err) {
        console.error("Failed to load example:", err);
      } finally {
        setLoading(false);
      }
    },
    [setDocument, setDiagnostics],
  );

  const handleValidate = useCallback(async () => {
    if (!document) return;
    try {
      const result = await api.validate(document);
      setDiagnostics(result.diagnostics, result.warnings);
    } catch (err) {
      console.error("Validation failed:", err);
    }
  }, [document, setDiagnostics]);

  const handleSave = useCallback(async () => {
    if (!document) return;
    try {
      const source = await api.unparse(document);
      const blob = new Blob([source], { type: "text/plain" });
      const url = URL.createObjectURL(blob);
      const a = window.document.createElement("a");
      a.href = url;
      const name = document.workflows?.[0]?.name || "workflow";
      a.download = `${name}.iter`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      console.error("Save failed:", err);
    }
  }, [document]);

  const handleCopySource = useCallback(async () => {
    if (!document) return;
    try {
      const source = await api.unparse(document);
      await navigator.clipboard.writeText(source);
    } catch (err) {
      console.error("Copy failed:", err);
    }
  }, [document]);

  return (
    <div className="flex items-center gap-3 px-4 h-full">
      <span className="font-bold text-sm tracking-wide">ITERION</span>
      <div className="h-4 w-px bg-gray-600" />

      <button
        className="bg-green-700 hover:bg-green-600 text-sm px-3 py-1 rounded"
        onClick={handleNew}
      >
        New
      </button>

      <select
        className="bg-gray-800 text-sm border border-gray-600 rounded px-2 py-1"
        onChange={(e) => loadExample(e.target.value)}
        defaultValue=""
        disabled={loading}
      >
        <option value="" disabled>
          Load Example...
        </option>
        {examples.map((name) => (
          <option key={name} value={name}>
            {name}
          </option>
        ))}
      </select>

      <div className="h-4 w-px bg-gray-600" />

      <button
        className="bg-blue-600 hover:bg-blue-700 text-sm px-3 py-1 rounded disabled:opacity-50"
        onClick={handleValidate}
        disabled={!document}
      >
        Validate
      </button>

      <button
        className="bg-indigo-600 hover:bg-indigo-700 text-sm px-3 py-1 rounded disabled:opacity-50"
        onClick={handleSave}
        disabled={!document}
      >
        Save
      </button>

      <button
        className="bg-gray-700 hover:bg-gray-600 text-sm px-3 py-1 rounded disabled:opacity-50"
        onClick={handleCopySource}
        disabled={!document}
      >
        Copy
      </button>

      <div className="h-4 w-px bg-gray-600" />

      <button
        className={`text-sm px-3 py-1 rounded ${
          sourceViewOpen ? "bg-purple-600 hover:bg-purple-700" : "bg-gray-700 hover:bg-gray-600"
        }`}
        onClick={toggleSourceView}
      >
        Source
      </button>

      {loading && <span className="text-xs text-gray-400">Loading...</span>}
    </div>
  );
}
