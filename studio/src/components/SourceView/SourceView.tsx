import { useCallback, useEffect, useRef, useState } from "react";
import Editor, { type Monaco } from "@/lib/monaco";
import { useDocumentStore, useDocumentStoreInstance } from "@/store/document";
import { useThemeStore } from "@/store/theme";
import * as api from "@/api/client";
import type { IterDocument } from "@/api/types";
import { ITER_LANGUAGE_ID, iterLanguageConfig, iterTokensProvider } from "@/lib/iterLanguage";
import { registerIterCompletionProvider } from "@/lib/iterMonacoCompletion";
import { applyParsedSource } from "@/lib/salvage";
import { useConfirm } from "@/hooks/useConfirm";
import { Button } from "@/components/ui/Button";
import { Select } from "@/components/ui/Select";
import { InlineBanner } from "@/components/ui/InlineBanner";

/** The picker's entry for the whole program of a bot in several files. It
 *  is not a file, so it can never collide with a `rel`, and it is
 *  read-only: one text cannot be split back into the files it came from. */
export const MERGED = "\u0000merged";

export default function SourceView() {
  const documentStore = useDocumentStoreInstance();
  const document = useDocumentStore((s) => s.document);
  const unit = useDocumentStore((s) => s.unit);
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setUnit = useDocumentStore((s) => s.setUnit);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const salvaged = useDocumentStore((s) => s.salvaged);
  const currentSource = useDocumentStore((s) => s.currentSource);
  const setCurrentSource = useDocumentStore((s) => s.setCurrentSource);
  const setSalvaged = useDocumentStore((s) => s.setSalvaged);
  const isDirty = useDocumentStore((s) => s.isDirty);
  const setSourceEditing = useDocumentStore((s) => s.setSourceEditing);
  const { confirm, dialog } = useConfirm();
  const [source, setSource] = useState("");
  const [editing, setEditingState] = useState(false);
  // Mirrored into the store: the watcher reads it to decide whether a file
  // changed on disk may be reloaded under this buffer.
  const setEditing = useCallback(
    (on: boolean) => {
      setEditingState(on);
      setSourceEditing(on);
    },
    [setSourceEditing],
  );
  const [parseError, setParseError] = useState<string | null>(null);
  const [refused, setRefused] = useState<string | null>(null);
  // Which file of the unit the view is on; null until a unit names one.
  const [selected, setSelected] = useState<string | null>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => setSourceEditing(false), [setSourceEditing]);
  // Every render this effect starts carries a generation. The cleanup bumps
  // it, so an answer that arrives after the author changed file — or
  // started typing — is dropped instead of landing in the editor. Without
  // it a late answer for file A lands while the picker says B, and the next
  // Apply writes A's text into B: the server cannot tell, since that text
  // parses.
  const renderGen = useRef(0);
  // The document this view last produced, and for which file. `isDirty()`
  // measures "the document moved since the last save"; what actually
  // matters here is "it holds an overlay for a file OTHER than this one".
  // They part on the commonest loop — edit, Apply, look, edit, Apply —
  // where re-applying the SAME file replaces only the text the author is
  // deliberately replacing, and a danger dialog every time after the first
  // would teach them to click through it.
  const lastApplied = useRef<{ rel: string; doc: IterDocument | null } | null>(null);

  // The unit's identity moves on every save and every external reload. An
  // explicit choice survives it; a selection whose file is GONE — deleted
  // from under the picker — falls back to the main rather than naming a
  // file nothing can answer for.
  useEffect(() => {
    if (!unit) {
      setSelected(null);
      return;
    }
    setSelected((cur) => {
      // A salvage is about the MAIN — it is the file that did not parse,
      // and the one the refusal on every write points here to repair. Left
      // on the merged entry the author would face a disabled picker with
      // no Edit: a refusal naming a control they cannot reach, which is
      // the defect this view was changed to remove.
      if (salvaged) return unit.main;
      if (cur === MERGED) return cur;
      if (cur && unit.files.some((f) => f.rel === cur)) return cur;
      return unit.main;
    });
  }, [unit, salvaged]);

  const perFile = !!unit && !!selected && selected !== MERGED && !salvaged;

  // Which file the buffer currently HOLDS the text of. `editable` says the
  // view is ABOUT a file a save could land on; it says nothing about what
  // is in the editor, and the two are apart for the whole debounce plus the
  // round trip after every switch. Entering the mode in that window froze
  // the PREVIOUS file's text there — the render effect returns early while
  // editing, so it never caught up — and Apply sent it under the newly
  // selected name. That text parses, so no server can tell.
  const bufferKey = `${currentFilePath ?? ""}\u0000${salvaged ? "!salvaged" : perFile ? selected : "!whole"}`;
  // STATE, not a ref: what the render reads has to be what re-renders it.
  // A ref written in the timer schedules nothing, and the only thing that
  // re-rendered was `setSource` — which React bails out of when the text
  // that lands is byte-identical to what is already shown. So a render
  // that changed nothing left `stale` true in the committed tree: Edit
  // gone, and no control inside this view to bring it back. Setting the
  // same key twice bails harmlessly; setting a new one always re-renders.
  const [renderedFor, setRenderedFor] = useState<string | null>(null);
  const stale = renderedFor !== bufferKey;

  // Sync document → source (when not in editing mode)
  useEffect(() => {
    if (editing) return;
    clearTimeout(debounceRef.current);
    const gen = ++renderGen.current;
    const key = bufferKey;
    debounceRef.current = setTimeout(async () => {
      // A salvaged document is the file MINUS the region the parser could
      // not read: rendering it back would show the author a text their own
      // file does not contain, and hide the very lines they have to fix. The
      // file's text is what is shown — the MAIN's, for a bot in several
      // files, which the selection effect above pins while salvaged, since
      // nothing rendered from a salvaged merge can be trusted either.
      //
      // For a bot in one file, editing it here is the way out: an Apply that
      // parses whole clears the flag and Save works again.
      if (salvaged) {
        setRenderedFor(key);
        setSource(currentSource ?? "");
        setParseError(null);
        setRefused(null);
        return;
      }
      if (!document) return;
      try {
        if (perFile && currentFilePath && selected) {
          // ONE file of the unit, rendered from the document by
          // provenance. A file the writer cannot reproduce comes back as
          // ITS OWN text with the reason — never the writer's, which would
          // be a text the author's file does not contain.
          const res = await api.unparseUnitFile(document, currentFilePath, selected);
          if (gen !== renderGen.current) return;
          setRenderedFor(key);
          setSource(res.source);
          setRefused(res.refused ?? null);
        } else {
          const res = await api.unparse(
            document,
            unit ? { flatten: true, path: currentFilePath } : { path: currentFilePath },
          );
          if (gen !== renderGen.current) return;
          setRenderedFor(key);
          setSource(res.source);
          setRefused(res.refused ?? null);
        }
        setParseError(null);
      } catch (err) {
        if (gen !== renderGen.current) return;
        // The server refuses to render a document the .bot syntax cannot
        // express as the same program (422) and says which declaration
        // it is; keep the last good source and show why it stopped.
        setParseError(err instanceof Error ? err.message : "The document cannot be rendered as .bot source");
      }
    }, 500);
    return () => {
      clearTimeout(debounceRef.current);
      renderGen.current++;
    };
  }, [document, editing, unit, salvaged, currentSource, perFile, selected, currentFilePath, bufferKey]);

  const handleApply = useCallback(async () => {
    // While salvaged this view shows the FILE's text, which does not carry
    // canvas edits — currentSource is the last opened/saved text, and a node
    // edit never touches it. Applying would replace the canvas with a text
    // that predates it, and the refusal on every write sends the author
    // HERE, so the loss sits on the guided path. Asked only in that case:
    // unsalvaged, the text is the document unparsed and there is nothing to
    // lose, and text-only edits leave the buffer clean.
    if (salvaged && isDirty()) {
      const go = await confirm({
        title: "Replace the canvas with this text?",
        message:
          "This is the file as it is on disk. It does not include the changes you made in the canvas, and applying replaces them.",
        confirmLabel: "Replace",
        confirmVariant: "danger",
      });
      if (!go) return;
    }
    // What this text was written against, snapshotted before any await —
    // every input, the DOCUMENT included. Three of the four were not
    // enough: the assistant's Apply-proposal moves the document under the
    // same file (`applyParsedSource` sets the document and the flag and
    // nothing else), so a buffer typed before it, applied after it, wrote
    // the pre-proposal text back over what the operator had just accepted
    // — text that parses, which no server can refuse. `salvaged` is here
    // for the same reason from the other side: it flips on its own and a
    // stale answer landing would clear the one flag that refuses writes.
    //
    // `selected` is not here because the picker is disabled while editing,
    // so it cannot move between the snapshot and the await.
    const was = { path: currentFilePath, unit, salvaged, document };
    const moved = () => {
      const now = documentStore.getState();
      return (
        now.currentFilePath !== was.path ||
        now.unit !== was.unit ||
        now.salvaged !== was.salvaged ||
        now.document !== was.document
      );
    };
    try {
      // A bot in several files is applied FILE BY FILE: the server
      // re-parses the unit with this one replaced and hands back the merged
      // document. It answers no revision — an overlay moved no file at rest
      // — so the revision this document was opened at is kept, and the next
      // save still detects a file a colleague changed meanwhile.
      if (unit && (!currentFilePath || !selected || selected === MERGED)) {
        setParseError(
          "This bot is in several files and the editor does not know which one this text belongs to. Reopen it from the files list.",
        );
        return;
      }
      if (unit && currentFilePath && selected && selected !== MERGED) {
        // The apply sends this ONE text and no document: the server rebuilds
        // the merged program from the bot's files AS STORED, plus this
        // overlay. So an unsaved canvas edit living in ANOTHER file of the
        // unit is replaced, not carried — and so is a per-file apply made
        // before this one and not yet saved. The salvage confirm above does
        // not cover it: its comment ("unsalvaged, the text is the document
        // unparsed and there is nothing to lose") is true of a bot in ONE
        // file, where the buffer IS the document, and false of a unit,
        // where the buffer is one file and the rest comes from storage.
        const ours =
          lastApplied.current !== null &&
          lastApplied.current.rel === selected &&
          lastApplied.current.doc === document;
        if (isDirty() && !ours) {
          const go = await confirm({
            title: "Rebuild this bot from its stored files?",
            message:
              "Applying rebuilds this bot from its files as they are stored, plus the text you edited here. Unsaved changes to this bot's other files are not included, and applying replaces them.",
            confirmLabel: "Rebuild",
            confirmVariant: "danger",
          });
          if (!go) return;
        }
        const result = await api.parseUnitFile(currentFilePath, selected, source);
        // The tab can change under this: the toolbar's own file picker is
        // not disabled by this view's Edit mode, and the assistant can
        // apply a proposal. The render effect's generation cannot stand in
        // for this guard — it is bumped by the document change this very
        // Apply causes.
        if (moved()) {
          setParseError("The editor changed while this was applying. Nothing was applied.");
          return;
        }
        applyParsedSource(result, { setDocument, setSalvaged });
        // What the STORE ends up holding, not what was handed to it: the
        // store normalises the document, so the object the next render
        // reads is not the one this answer carried.
        lastApplied.current = { rel: selected, doc: documentStore.getState().document };
        if (result.unit) setUnit({ ...unit, files: result.unit.files });
        setDiagnostics(result.diagnostics);
        // Only the main's. currentSource is what a cloud launch and every
        // resume send inline AS the program (store/document.ts): a
        // fragment declares no workflow, and putting one there would make
        // them run something that is not this bot.
        if (selected === unit.main) setCurrentSource(source);
        setParseError(null);
        setEditing(false);
        return;
      }
      const result = await api.parseSource(source);
      if (moved()) {
        setParseError("The editor changed while this was applying. Nothing was applied.");
        return;
      }
      // The way out of a salvage, and the only one: text that parses whole
      // makes the document the program again, so a save may write it. The
      // canvas cannot do this — it never held the region the parser could
      // not read — which is why the refusal points here.
      applyParsedSource(result, { setDocument, setSalvaged });
      setDiagnostics(result.diagnostics);
      // The applied text becomes the buffer's own. A repair that does not
      // parse YET leaves the document a salvage, and the sync above would
      // otherwise put the text this view opened with back over what the
      // author just typed — the loss, inside the way out of it.
      setCurrentSource(source);
      setParseError(null);
      setEditing(false);
    } catch (err) {
      setParseError(err instanceof Error ? err.message : "Parse failed");
    }
  }, [
    source,
    setDocument,
    setDiagnostics,
    setSalvaged,
    setCurrentSource,
    setUnit,
    salvaged,
    isDirty,
    confirm,
    unit,
    salvaged,
    currentFilePath,
    selected,
    documentStore,
    document,
  ]);

  const handleEditorWillMount = useCallback((monaco: Monaco) => {
    if (!monaco.languages.getLanguages().some((l: { id: string }) => l.id === ITER_LANGUAGE_ID)) {
      monaco.languages.register({ id: ITER_LANGUAGE_ID });
      monaco.languages.setLanguageConfiguration(ITER_LANGUAGE_ID, iterLanguageConfig);
      monaco.languages.setMonarchTokensProvider(ITER_LANGUAGE_ID, iterTokensProvider);
    }
    registerIterCompletionProvider(monaco);
  }, []);

  // Editable when the view is ABOUT a file a save could land on: a bot in
  // one file — salvaged or not, since repairing it here is the way out —
  // or one file of a unit whose main parses. Not the merged program, which
  // cannot be split back into the files it came from; not a file the
  // writer cannot reproduce, whose save is refused too; and not a salvaged
  // unit, which the render below says in its own words.
  const editable = (!unit || perFile) && !refused;

  // Leaving edit mode is not optional. `editing` is this component's own
  // state while `unit`, `salvaged` and the selection are the store's, which
  // other surfaces move — opening another bot mid-edit, above all. Every
  // branch that makes the view read-only also hides Apply and Cancel, so
  // without this there is no control left to leave the mode: the editor
  // stays writable (`readOnly: !editing`) under a label saying it is not,
  // the sync below keeps returning early, and the previous bot's buffer
  // sits on screen as though it were this one's file. One place rather
  // than three guards — it covers the salvaged unit, the merged program
  // and a refused file alike.
  useEffect(() => {
    if (!editable || stale) setEditing(false);
  }, [editable, stale, setEditing]);

  return (
    <div className="h-full flex flex-col">
      <div className="flex items-center justify-between px-2 py-1 bg-surface-1 border-b border-border-default shrink-0 gap-2">
        {unit ? (
          <label className="flex items-center gap-1 text-xs text-fg-subtle min-w-0">
            <span className="shrink-0">File</span>
            <Select
              fit
              aria-label="File of this bot"
              data-testid="source-view-file-picker"
              className="max-w-[18rem]"
              value={selected ?? unit.main}
              disabled={editing || salvaged}
              onChange={(e) => {
                setSelected(e.target.value);
                setRefused(null);
                setParseError(null);
              }}
            >
              {unit.files.map((f) => (
                <option key={f.rel} value={f.rel}>
                  {f.rel}
                  {f.rel === unit.main ? " (main)" : ""}
                </option>
              ))}
              <option value={MERGED}>Merged program of {unit.files.length} files (read-only)</option>
            </Select>
          </label>
        ) : (
          <span className="text-xs text-fg-subtle">.bot Source</span>
        )}
        <div className="flex gap-2 shrink-0">
          {unit && salvaged ? (
            <span className="text-xs text-fg-subtle" data-testid="source-view-salvaged-unit-note">
              Read-only: this bot&apos;s main did not parse. It is saved from its files as they
              are stored, so the main has to be repaired there.
            </span>
          ) : unit && selected === MERGED ? (
            <span className="text-xs text-fg-subtle" data-testid="source-view-unit-note">
              Read-only: the merged program is not a file. Pick a file above to edit it.
            </span>
          ) : !editable || stale ? null : !editing ? (
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
              <Button variant="primary" size="sm" onClick={handleApply}>
                Apply
              </Button>
              <Button variant="secondary" size="sm" onClick={() => setEditing(false)}>
                Cancel
              </Button>
            </>
          )}
        </div>
      </div>
      {refused && (
        <InlineBanner tone="warning" layout="sticky">
          {selected === MERGED
            ? `This is the merged program of ${unit?.files.length ?? 0} files, and it is not what one of them holds: `
            : "This is the file as it is stored, and it cannot be edited here: "}
          {refused}
        </InlineBanner>
      )}
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
      {dialog}
    </div>
  );
}
