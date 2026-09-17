import { useCallback, useEffect, useRef, useState } from "react";
import Editor, { type Monaco } from "@/lib/monaco";
import { useDocumentStore } from "@/store/document";
import { useThemeStore } from "@/store/theme";
import * as api from "@/api/client";
import { ITER_LANGUAGE_ID, iterLanguageConfig, iterTokensProvider } from "@/lib/iterLanguage";
import { registerIterCompletionProvider } from "@/lib/iterMonacoCompletion";
import { applyParsedSource } from "@/lib/salvage";
import { Button } from "@/components/ui/Button";

export default function SourceView() {
  const document = useDocumentStore((s) => s.document);
  const unit = useDocumentStore((s) => s.unit);
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const salvaged = useDocumentStore((s) => s.salvaged);
  const currentSource = useDocumentStore((s) => s.currentSource);
  const setSalvaged = useDocumentStore((s) => s.setSalvaged);
  const [source, setSource] = useState("");
  const [editing, setEditing] = useState(false);
  const [parseError, setParseError] = useState<string | null>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  // Sync document → source (when not in editing mode)
  useEffect(() => {
    if (editing) return;
    clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(async () => {
      // A salvaged document is the file MINUS the region the parser could
      // not read: rendering it back would show the author a text their own
      // file does not contain, and hide the very lines they have to fix. The
      // file's text is what is shown, and editing it here is the way out —
      // an Apply that parses whole clears the flag and Save works again.
      if (salvaged) {
        setSource(currentSource ?? "");
        setParseError(null);
        return;
      }
      if (!document) return;
      try {
        const result = await api.unparse(document, unit ? { flatten: true } : undefined);
        setSource(result);
        setParseError(null);
      } catch (err) {
        // The server refuses to render a document the .bot syntax cannot
        // express as the same program (422) and says which declaration
        // it is; keep the last good source and show why it stopped.
        setParseError(err instanceof Error ? err.message : "The document cannot be rendered as .bot source");
      }
    }, 500);
    return () => clearTimeout(debounceRef.current);
  }, [document, editing, unit, salvaged, currentSource]);

  const handleApply = useCallback(async () => {
    try {
      const result = await api.parseSource(source);
      // The way out of a salvage, and the only one: text that parses whole
      // makes the document the program again, so a save may write it. The
      // canvas cannot do this — it never held the region the parser could
      // not read — which is why the refusal points here.
      applyParsedSource(result, { setDocument, setSalvaged });
      setDiagnostics(result.diagnostics);
      setParseError(null);
      setEditing(false);
    } catch (err) {
      setParseError(err instanceof Error ? err.message : "Parse failed");
    }
  }, [source, setDocument, setDiagnostics, setSalvaged]);

  const handleEditorWillMount = useCallback((monaco: Monaco) => {
    if (!monaco.languages.getLanguages().some((l: { id: string }) => l.id === ITER_LANGUAGE_ID)) {
      monaco.languages.register({ id: ITER_LANGUAGE_ID });
      monaco.languages.setLanguageConfiguration(ITER_LANGUAGE_ID, iterLanguageConfig);
      monaco.languages.setMonarchTokensProvider(ITER_LANGUAGE_ID, iterTokensProvider);
    }
    registerIterCompletionProvider(monaco);
  }, []);

  return (
    <div className="h-full flex flex-col">
      <div className="flex items-center justify-between px-2 py-1 bg-surface-1 border-b border-border-default shrink-0">
        <span className="text-xs text-fg-subtle">
          {unit ? `Merged program of ${unit.files.length} files` : ".bot Source"}
        </span>
        <div className="flex gap-2">
          {unit ? (
            <span className="text-xs text-fg-subtle" data-testid="source-view-unit-note">
              Read-only: a bot in several files is edited file by file — open each file from the files drawer, or use the canvas.
            </span>
          ) : !editing ? (
            <Button
              variant="ghost"
              size="sm"
              className="text-accent-text hover:underline"
              onClick={() => setEditing(true)}
            >
              Edit
            </Button>
          ) : (
            <>
              <Button
                variant="primary"
                size="sm"
                onClick={handleApply}
              >
                Apply
              </Button>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setEditing(false)}
              >
                Cancel
              </Button>
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
