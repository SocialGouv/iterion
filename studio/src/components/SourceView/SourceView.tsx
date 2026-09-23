import { useCallback, useEffect, useRef, useState } from "react";
import Editor from "@/lib/monaco";
import { unreachableSourceBuffer, useDocumentStore, useDocumentStoreInstance } from "@/store/document";
import { useThemeStore } from "@/store/theme";
import { useUIStore } from "@/store/ui";
import * as api from "@/api/client";
import { parseBotSourceEditorPath } from "@/api/client";
import type { IterDocument } from "@/api/types";
import { ITER_LANGUAGE_ID } from "@/lib/iterLanguage";
import { registerIterLanguage } from "@/lib/iterMonaco";
import { applyParsedSource } from "@/lib/salvage";
import { useConfirm } from "@/hooks/useConfirm";
import { Button } from "@/components/ui/Button";
import { Select } from "@/components/ui/Select";
import { InlineBanner } from "@/components/ui/InlineBanner";

/** The picker's entry for the whole program of a bot in several files.
 *  Not a path: a unit's `rel` is a slash path ending in `.bot`, so this
 *  cannot collide with one. Deliberately plain ASCII — it travels as an
 *  `<option value>` and back through `e.target.value`, and a NUL there
 *  rests on a round trip no test in this repo exercises in a real browser
 *  (#1649). It is read-only either way: one text cannot be split back
 *  into the files it came from. */
export const MERGED = "<merged>";

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
  const setSourceBuffer = useDocumentStore((s) => s.setSourceBuffer);
  const { confirm, dialog } = useConfirm();
  const addToast = useUIStore((s) => s.addToast);
  const [source, setSource] = useState("");
  const [editing, setEditingState] = useState(false);
  // The text the buffer was rendered FROM, frozen when the mode opens. It is
  // what `text !== base` compares against, so every surface outside can tell
  // an open editor from one holding work a discard would take.
  const baseRef = useRef("");
  // Published to the store, not merely flagged there: the file watcher, the
  // assistant's reload-after-write, the tab close and File → New all decide
  // whether to take this text, and a boolean could only tell them the editor
  // was open, never whether anything was in it (#1662).
  const setEditing = useCallback(
    (on: boolean) => {
      setEditingState(on);
      if (on) {
        baseRef.current = sourceRef.current;
        setSourceBuffer({
          path: pathRef.current,
          rel: relRef.current,
          text: sourceRef.current,
          base: sourceRef.current,
          doc: renderedRef.current?.doc ?? documentRef.current,
        });
      } else {
        setSourceBuffer(null);
      }
    },
    [setSourceBuffer],
  );
  // Read inside `setEditing` and the editor's onChange, both of which must
  // see the CURRENT text and file without re-creating themselves on every
  // keystroke — a new onChange identity per character remounts nothing but
  // costs a render of the editor for each one.
  const sourceRef = useRef("");
  sourceRef.current = source;
  const pathRef = useRef<string | null>(null);
  pathRef.current = currentFilePath;
  const [parseError, setParseError] = useState<string | null>(null);
  const [refused, setRefused] = useState<string | null>(null);
  // Which file of the unit the view is on; null until a unit names one.
  const [selected, setSelected] = useState<string | null>(null);
  const relRef = useRef<string | null>(null);
  relRef.current = selected;
  const documentRef = useRef<IterDocument | null>(null);
  documentRef.current = document;
  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  // The buffer OUTLIVES this mount when it holds work. The view is a
  // half-pane inside a tab host inside a route: hiding the pane, expanding
  // the canvas, leaving /editor, clicking the sidebar's Editor entry — every
  // one of those unmounts it, and dropping the buffer there destroyed the
  // author's text with no prompt. Three rounds of guarding those exits found
  // a further one each time; keeping the buffer removes the exits instead,
  // and the mount below adopts it back. A clean buffer is dropped: it holds
  // nothing, and leaving it would keep the watcher from ever auto-reloading.
  useEffect(
    () => () => {
      if (!documentStore.getState().isSourceDirty()) setSourceBuffer(null);
    },
    [documentStore, setSourceBuffer],
  );
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
      // The selection is component state and does not survive the unmount,
      // so a buffer held for a FRAGMENT would come back to a view sitting on
      // the main and never be adopted. Land on the file the held work is
      // for, when the unit still has it.
      const held = documentStore.getState().sourceBuffer;
      if (
        held &&
        held.text !== held.base &&
        held.path === currentFilePath &&
        held.rel &&
        unit.files.some((f) => f.rel === held.rel)
      ) {
        return held.rel;
      }
      return unit.main;
    });
  }, [unit, salvaged, currentFilePath, documentStore]);

  const perFile = !!unit && !!selected && selected !== MERGED && !salvaged;
  // Which twin this tab is on. The control that repairs a main the canvas
  // cannot open differs between them: a local author edits the file where
  // it lives, a cloud author opens it as text from the bundle's files list
  // (#1659). Naming the wrong one sends them to a surface they do not have.
  const onCloudBundle = !!currentFilePath && !!parseBotSourceEditorPath(currentFilePath);

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
  const [rendered, setRendered] = useState<{ key: string; doc: IterDocument | null } | null>(null);
  const renderedRef = useRef<{ key: string; doc: IterDocument | null } | null>(null);
  renderedRef.current = rendered;
  // `stale` keeps the FILE axis only. Adding the document here would eject
  // the author from edit mode on every canvas keystroke; what the document
  // decides is whether the buffer may be WRITTEN, which is moved()'s job.
  const stale = rendered?.key !== bufferKey;

  // Adopt a buffer this view left behind. The pane is mounted and unmounted
  // by three nesting conditions it does not own (the view toggle, the canvas
  // expand, the active tab, the route), so an author who hides it mid-edit
  // gets their text BACK rather than a prompt asking whether to lose it.
  // Once only, and only for the same file: the buffer lives in THIS tab's
  // store, so a path that no longer matches means this tab has moved file
  // (Save As, File → New, an open) and the text is not about what is on
  // screen.
  const adopted = useRef(false);
  useEffect(() => {
    if (adopted.current || editing) return;
    const held = documentStore.getState().sourceBuffer;
    if (!held || held.text === held.base) return;
    if (held.path !== currentFilePath) return;
    if (held.rel !== (unit ? selected : null)) return;
    adopted.current = true;
    baseRef.current = held.base;
    setSource(held.text);
    // The provenance comes back with the text, not reset to the current
    // document: if the document moved while the pane was shut, the Apply
    // must still be refused.
    setRendered({ key: bufferKey, doc: held.doc });
    // Not through `setEditing`: that publishes a FRESH buffer whose base is
    // the text on screen, which would mark the author's work clean.
    setEditingState(true);
  }, [editing, currentFilePath, selected, unit, bufferKey, documentStore]);

  // A held buffer typed for a file this tab no longer has — its file left the
  // unit, or the unit came or went under it — can never be adopted, and
  // holding it keeps the watcher from ever auto-reloading this tab and lights
  // `beforeunload` for text no surface can show. Say so and let go, rather
  // than keep work nobody can reach. Save As asks the same question, so the
  // two cannot disagree about which text is kept.
  useEffect(() => {
    if (editing) return;
    const held = documentStore.getState().sourceBuffer;
    if (!held || held.text === held.base) return;
    const unreachable = unreachableSourceBuffer(held, currentFilePath, unit);
    if (!unreachable) return;
    setSourceBuffer(null);
    addToast(unreachable, "warning", { persistent: true });
  }, [editing, unit, currentFilePath, documentStore, setSourceBuffer, addToast]);

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
        setRendered({ key, doc: document });
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
          setRendered({ key, doc: document });
          setSource(res.source);
          setRefused(res.refused ?? null);
        } else {
          const res = await api.unparse(
            document,
            unit ? { flatten: true, path: currentFilePath } : { path: currentFilePath },
          );
          if (gen !== renderGen.current) return;
          setRendered({ key, doc: document });
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
    // The document the BUFFER was rendered from, not the one current when
    // Apply was clicked: a change landing between the last completed
    // render and the click leaves `stale` false (it carries no document)
    // and Edit clickable, and entering the mode cancels the pending
    // re-render — so the stale text freezes and Apply would write it back
    // as that file's whole content, reverting the change with no
    // diagnostic. While salvaged the buffer is the FILE's text, not a
    // render, and the confirm above already governs replacing the canvas.
    const was = {
      path: currentFilePath,
      unit,
      salvaged,
      document: salvaged ? document : (rendered?.doc ?? document),
    };
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
        const applied = documentStore.getState().document;
        lastApplied.current = { rel: selected, doc: applied };
        // The buffer now corresponds to THIS document for this file — it
        // is the text that produced it. Without this the render
        // provenance would stay on the pre-apply document for the whole
        // debounce, and a second apply to the same file would be refused
        // as stale although nothing had moved under it.
        setRendered({ key: bufferKey, doc: applied });
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
      // The provenance follows the apply, as it does on the per-file branch:
      // without it the buffer keeps naming the PRE-apply document for the
      // whole debounce, and a second Apply to the same file is refused as
      // stale although nothing but this view had moved it.
      setRendered({ key: bufferKey, doc: documentStore.getState().document });
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
    rendered,
  ]);

  // Cancel takes the typed text, and there is no undo for it — the render
  // effect overwrites the buffer 500 ms after the mode closes. The prompt is
  // asked HERE because it is the one discard the author triggers from inside
  // this view; the ones triggered from outside (tab close, File → New, the
  // watcher's reload, the assistant's reload-after-write) consult
  // `hasUnsavedWork()` before they move the store.
  const handleCancel = useCallback(async () => {
    if (source !== baseRef.current) {
      const go = await confirm({
        title: "Discard this text?",
        message:
          "What you typed here has not been applied. Closing the editor replaces it with the file as it is now.",
        confirmLabel: "Discard",
        confirmVariant: "danger",
      });
      if (!go) return;
    }
    setEditing(false);
  }, [source, confirm, setEditing]);

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
  // Leaving the mode is not the same as discarding the work. This effect
  // closes edit mode when the view turns read-only under it — another bot
  // opened, a salvage appeared, a unit arrived — and it must leave the text
  // where a discard path can still see it, exactly as the unmount cleanup
  // does. Going through `setEditing(false)` dropped the buffer, which made
  // the author watch a repair vanish under a read-only editor with no Apply,
  // no Cancel and no undo.
  //
  // When the view is still editable — it only moved to another key, as Save
  // As moves it to a new path — the kept text must be adopted again, so the
  // once-per-mount latch is released. Left set, a second Save As (or one
  // after the pane was hidden and shown) showed the rendered file under an
  // Edit button while the text stayed held, and that Edit replaced it. Not
  // when read-only: adoption would re-enter edit mode, and this effect would
  // leave it again, for ever.
  useEffect(() => {
    if (!editing || (editable && !stale)) return;
    setEditingState(false);
    if (!documentStore.getState().isSourceDirty()) setSourceBuffer(null);
    else if (editable) adopted.current = false;
  }, [editing, editable, stale, documentStore, setSourceBuffer]);

  return (
    <div className="h-full flex flex-col">
      <div className="flex items-center justify-between px-2 py-1 bg-surface-1 border-b border-border-default shrink-0 gap-2">
        {unit ? (
          <label className="flex min-w-0 items-center gap-1 text-xs text-fg-subtle">
            {/* The picker deliberately does NOT pass `fit`. `fit` makes its
                wrapper `inline-block`, which sizes to the longest option
                (248 px, the merged entry) and overflows the label; the
                wrapper is `position: relative` — it overlays the chevron —
                so the overflow painted ABOVE the static Apply/Cancel buttons
                and swallowed their clicks. Without `fit` the wrapper is
                `w-full` and shrinks with the pane. Clipping the label
                instead would ALSO bound it, and was measured erasing the
                picker to zero painted pixels in the merged state, under a
                note telling the author to pick a file above. */}
            <span className="shrink-0">File</span>
            <Select
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
        {/* `min-w-0` rather than `shrink-0`: the two read-only notes below are
            long (the salvage one runs to ~950 px), and a row that refuses to
            shrink them takes the picker's width instead — measured erasing it
            entirely in the merged state. The buttons branch keeps its natural
            width, being three short labels. */}
        <div className="flex min-w-0 gap-2">
          {unit && salvaged ? (
            <span
              className="truncate text-xs text-fg-subtle"
              title="Read-only: this bot's main did not parse."
              data-testid="source-view-salvaged-unit-note"
            >
              Read-only: this bot&apos;s main did not parse. It is saved from its files as they
              are stored, so the main has to be repaired there —{" "}
              {onCloudBundle
                ? "open it as text from the bundle's files list."
                : "edit the file where this bot's files live, then reopen it."}
            </span>
          ) : unit && selected === MERGED ? (
            <span
              className="truncate text-xs text-fg-subtle"
              title="Read-only: the merged program is not a file. Pick a file above to edit it."
              data-testid="source-view-unit-note"
            >
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
              <Button variant="secondary" size="sm" onClick={() => void handleCancel()}>
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
          beforeMount={registerIterLanguage}
          value={source}
          onChange={(v) => {
            if (!editing) return;
            const text = v ?? "";
            setSource(text);
            setSourceBuffer({
              path: pathRef.current,
              rel: relRef.current,
              text,
              base: baseRef.current,
              doc: renderedRef.current?.doc ?? documentRef.current,
            });
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
