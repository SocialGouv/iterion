import { useCallback, useEffect, useRef, useState } from "react";
import Editor, { type Monaco } from "@monaco-editor/react";
import { useDocumentStore } from "@/store/document";
import * as api from "@/api/client";
import { ITER_LANGUAGE_ID, iterLanguageConfig, iterTokensProvider } from "@/lib/iterLanguage";

export default function SourceView() {
  const document = useDocumentStore((s) => s.document);
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const [source, setSource] = useState("");
  const [editing, setEditing] = useState(false);
  const [parseError, setParseError] = useState<string | null>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  // Sync document → source (when not in editing mode)
  useEffect(() => {
    if (editing || !document) return;
    clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(async () => {
      try {
        const result = await api.unparse(document);
        setSource(result);
        setParseError(null);
      } catch {
        // silently ignore unparse errors during sync
      }
    }, 500);
    return () => clearTimeout(debounceRef.current);
  }, [document, editing]);

  const handleApply = useCallback(async () => {
    try {
      const result = await api.parseSource(source);
      setDocument(result.document);
      setDiagnostics(result.diagnostics);
      setParseError(null);
      setEditing(false);
    } catch (err) {
      setParseError(err instanceof Error ? err.message : "Parse failed");
    }
  }, [source, setDocument, setDiagnostics]);

  const handleEditorWillMount = useCallback((monaco: Monaco) => {
    if (!monaco.languages.getLanguages().some((l: { id: string }) => l.id === ITER_LANGUAGE_ID)) {
      monaco.languages.register({ id: ITER_LANGUAGE_ID });
      monaco.languages.setLanguageConfiguration(ITER_LANGUAGE_ID, iterLanguageConfig);
      monaco.languages.setMonarchTokensProvider(ITER_LANGUAGE_ID, iterTokensProvider);
    }
  }, []);

  return (
    <div className="h-full flex flex-col">
      <div className="flex items-center justify-between px-2 py-1 bg-gray-800 border-b border-gray-700 shrink-0">
        <span className="text-xs text-gray-400">.iter Source</span>
        <div className="flex gap-2">
          {!editing ? (
            <button
              className="text-xs text-blue-400 hover:text-blue-300"
              onClick={() => setEditing(true)}
            >
              Edit
            </button>
          ) : (
            <>
              <button
                className="text-xs text-green-400 hover:text-green-300"
                onClick={handleApply}
              >
                Apply
              </button>
              <button
                className="text-xs text-gray-400 hover:text-gray-300"
                onClick={() => setEditing(false)}
              >
                Cancel
              </button>
            </>
          )}
        </div>
      </div>
      {parseError && (
        <div className="px-2 py-1 bg-red-900/50 text-red-300 text-xs">{parseError}</div>
      )}
      <div className="flex-1 min-h-0">
        <Editor
          height="100%"
          language={ITER_LANGUAGE_ID}
          theme="vs-dark"
          beforeMount={handleEditorWillMount}
          value={source}
          onChange={(v) => {
            if (editing) setSource(v ?? "");
          }}
          options={{
            readOnly: !editing,
            minimap: { enabled: false },
            fontSize: 12,
            lineNumbers: "on",
            scrollBeyondLastLine: false,
            wordWrap: "on",
            automaticLayout: true,
          }}
        />
      </div>
    </div>
  );
}
