import { useCallback, useEffect, useRef, useState } from "react";
import Editor, { type Monaco } from "@monaco-editor/react";
import { useDocumentStore } from "@/store/document";
import { useThemeStore } from "@/store/theme";
import * as api from "@/api/client";
import { ITER_LANGUAGE_ID, iterLanguageConfig, iterTokensProvider } from "@/lib/iterLanguage";

export default function SourceView() {
  const document = useDocumentStore((s) => s.document);
  const resolvedTheme = useThemeStore((s) => s.resolved);
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
      <div className="flex items-center justify-between px-2 py-1 bg-surface-1 border-b border-border-default shrink-0">
        <span className="text-xs text-fg-subtle">.iter Source</span>
        <div className="flex gap-2">
          {!editing ? (
            <button
              className="text-xs text-accent hover:text-accent"
              onClick={() => setEditing(true)}
            >
              Edit
            </button>
          ) : (
            <>
              <button
                className="text-xs text-success hover:text-success-fg"
                onClick={handleApply}
              >
                Apply
              </button>
              <button
                className="text-xs text-fg-subtle hover:text-fg-muted"
                onClick={() => setEditing(false)}
              >
                Cancel
              </button>
            </>
          )}
        </div>
      </div>
      {parseError && (
        <div className="px-2 py-1 bg-danger-soft text-danger-fg text-xs">{parseError}</div>
      )}
      <div className="flex-1 min-h-0">
        <Editor
          height="100%"
          language={ITER_LANGUAGE_ID}
          theme={resolvedTheme === "dark" ? "vs-dark" : "vs"}
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
