import type { Monaco } from "@monaco-editor/react";
import type { editor, languages, Position } from "monaco-editor";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { computeRefs, REF_GROUP_ORDER } from "@/lib/refCompletion";
import { ITER_LANGUAGE_ID } from "@/lib/iterLanguage";

const SORT_PREFIX: Record<string, string> = {
  input: "1",
  vars: "2",
  outputs: "3",
  sessions: "4",
  artifacts: "5",
};

let registered = false;

/**
 * Register a Monaco completion provider for `.iter` source that suggests
 * `{{...}}` references from the live document state. Idempotent — safe to
 * call on every editor mount.
 */
export function registerIterCompletionProvider(monaco: Monaco) {
  if (registered) return;
  registered = true;

  monaco.languages.registerCompletionItemProvider(ITER_LANGUAGE_ID, {
    triggerCharacters: ["{", "."],
    provideCompletionItems(model: editor.ITextModel, position: Position) {
      const doc = useDocumentStore.getState().document;
      const activeWorkflowName = useUIStore.getState().activeWorkflowName ?? undefined;
      if (!doc) return { suggestions: [] };

      const lineText = model.getLineContent(position.lineNumber);
      const upToCaret = lineText.slice(0, position.column - 1);
      // Find the most recent `{{` on this line.
      const tokenStart = upToCaret.lastIndexOf("{{");
      if (tokenStart < 0) return { suggestions: [] };
      const tokenEnd = upToCaret.indexOf("}}", tokenStart);
      if (tokenEnd >= 0 && tokenEnd < upToCaret.length) return { suggestions: [] };
      const queryStart = tokenStart + 2;
      const partial = upToCaret.slice(queryStart);
      // Bail if the partial contains whitespace (multi-token expressions).
      if (/[\s{}]/.test(partial)) return { suggestions: [] };

      const refs = computeRefs(doc, { kind: "monaco" }, activeWorkflowName);
      const range = {
        startLineNumber: position.lineNumber,
        endLineNumber: position.lineNumber,
        startColumn: tokenStart + 1, // 1-based, inclusive of '{{'
        endColumn: position.column,
      };

      const suggestions: languages.CompletionItem[] = refs.map((r) => ({
        label: r.label,
        kind: monaco.languages.CompletionItemKind.Variable,
        insertText: r.value,
        detail: r.detail ? `${r.group} · ${r.detail}` : r.group,
        range,
        sortText: `${SORT_PREFIX[r.group] ?? "9"}${r.label}`,
        filterText: r.label,
      }));

      // Stable group ordering for headers (Monaco doesn't render groups but
      // sortText keeps them clustered).
      void REF_GROUP_ORDER;

      return { suggestions };
    },
  });
}
