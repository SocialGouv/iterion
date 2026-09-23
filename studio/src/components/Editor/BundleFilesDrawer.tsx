import { useEffect, useMemo, useRef, useState } from "react";
import Editor from "@/lib/monaco";
import { FileIcon, PlusIcon, TrashIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import { ApiError } from "@/api/client";
import {
  deleteBotSourceFile,
  getBotSource,
  putBotSourceFile,
  type BotSourceFull,
} from "@/api/botSources";
import { botSourceEditorPath } from "@/api/client";
import { Button, Drawer, Spinner } from "@/components/ui";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useConfirm } from "@/hooks/useConfirm";
import { usePromptText } from "@/hooks/usePromptText";
import { inferMonacoLanguage } from "@/lib/inferMonacoLanguage";
import { ITER_LANGUAGE_ID as ITER_LANGUAGE } from "@/lib/iterLanguage";
import { registerIterLanguage } from "@/lib/iterMonaco";
import { isWorkflowFile } from "@/lib/workflowFile";
import { toastError } from "@/lib/errorHints";
import { useTabsStore } from "@/store/tabs";
import { useThemeStore } from "@/store/theme";
import { useUIStore } from "@/store/ui";
import { referenceDragProps } from "@/lib/chatDock/dragReference";

interface Props {
  teamID: string;
  slug: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

const MAIN_BOT = "main.bot";

// BundleFilesDrawer is the multi-file editor for a team-authored bot bundle.
// The bundle's main.bot is the DSL workflow — clicking it focuses/opens the
// Canvas editor tab (the single owner of .bot rendering). Every other file
// (skills/*.md, manifest.yaml, prompts/…) is plain text edited inline in a
// Monaco buffer and saved per-file to the bot-source store. New files (a fresh
// skill) can be added, and non-main files removed.
//
// This buffer belongs to "would this take the author's work?" and is one of
// the members the document store cannot answer for: it is this component's
// own state, so `hasUnsavedWork()` — which the store-backed members consult
// — does not see it. (`Runs/FileEditDialog` holds a buffer of the same
// shape and has no gate at all; that is its own ticket, not this file's.)
//
// The drawer owns a gate, `leavingBuffer`, for the exits it can see:
// closing the drawer (Escape, outside click, Close), Back, and following the
// props to another bot (#1749). Those three are gated here.
//
// The RULE, not a count: **any ancestor that conditionally renders `Toolbar`
// destroys this buffer**, and no gate in this file can reach one. Measured
// examples — the parent dropping `bundleRef` when the editor's file stops
// being a `botsource://` path (which `Toolbar.tsx` latches against),
// `DesktopOnlyNotice` swapping the grid out on a narrow viewport or a zoom,
// `expanded` unmounting the toolbar, and `EditorTabHost` replacing the whole
// view while a tab hydrates or errors. That set is open-ended, so counting it
// is a claim that goes stale the moment it is written.
//
// The class closes only by taking the buffer OUT of component state — keyed
// by teamID/slug/rel in a store, with the drawer as a view of it, the way the
// Source view's buffer already works. That is its own change (#1755), and
// `Runs/FileEditDialog` holds the same shape with no gate at all.
//
// `onOpenChange` is a REQUEST, not the close: the parent owns `open`, so the
// gate withholds it until the author answers — asking first and closing
// anyway would unmount the buffer the dialog is asking about.
export default function BundleFilesDrawer({ teamID, slug, open, onOpenChange }: Props) {
  const [, setLocation] = useLocation();
  const addToast = useUIStore((s) => s.addToast);
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const { confirm, dismiss, dialog: confirmDialog } = useConfirm();
  const { prompt, dialog: promptDialog } = usePromptText();

  // Stamped with the pair it was READ from. `shown` alone was not enough: a
  // save in flight when the editor moves answers after the rebind, and an
  // unstamped bundle then showed one bot's files under another's title and
  // carried the first's if-match token into a write aimed at the second —
  // which two bundles at the same version (two fresh forks, both at 1) would
  // not even refuse.
  const [loaded, setLoaded] = useState<{
    at: { teamID: string; slug: string };
    data: BotSourceFull;
  } | null>(null);
  const [loading, setLoading] = useState(false);
  const [editing, setEditing] = useState<{
    rel: string;
    value: string;
    original: string;
    /** A file that does not exist yet: Save is offered on it even empty. */
    created?: boolean;
  } | null>(null);
  const [saving, setSaving] = useState(false);
  const [busyRel, setBusyRel] = useState<string | null>(null);
  // Set when the store refused a write because the bundle moved under this
  // drawer. It is NOT cleared by re-fetching in the background: the token
  // this drawer holds is the one it READ, and silently adopting a fresh one
  // would turn the refusal into the overwrite it just prevented. It is
  // cleared by an explicit reload, which says what it discards, and by the
  // load effect, which re-reads the bundle in the same breath.
  const [conflict, setConflict] = useState(false);

  // The bundle this drawer is SHOWING, which is not always the one its props
  // name: the props follow the active editor file and can move while the
  // drawer is open, and the typed buffer belongs to the bundle it was read
  // from. The drawer follows the props only once there is nothing to lose,
  // or once the author has said so.
  const [bound, setBound] = useState<{ teamID: string; slug: string } | null>(null);
  // A props pair the author refused to follow. Without it the effect below
  // re-asks on every render for as long as the drawer stays behind.
  const declined = useRef<string | null>(null);

  // What the drawer is acting on. Every read, every write and every path it
  // hands out names THIS, not the props: once the author keeps their text the
  // two part, and a delete aimed at the props would hit the wrong bot.
  const shown = bound ?? { teamID, slug };

  // The guard is on the WRITE, not the read: an answer that names a pair the
  // drawer has left is DROPPED, so it can never replace the bundle actually
  // on screen. Nulling the read instead would have thrown away a good bundle
  // during an ordinary rebind and rendered the "could not be read" refusal
  // over a read that succeeded.
  const bundle = loaded?.data ?? null;
  const shownRef = useRef(shown);
  useEffect(() => {
    shownRef.current = shown;
  });
  const setBundle = (data: BotSourceFull, at: { teamID: string; slug: string }) => {
    const now = shownRef.current;
    if (at.teamID !== now.teamID || at.slug !== now.slug) return;
    setLoaded({ at, data });
  };

  const bufferIsDirty = () => !!editing && editing.value !== editing.original;
  // A VALUE in the follow effect's deps, not just the ref: the latch below is
  // released when there is nothing left to lose, and an effect that never
  // re-runs after a save could never release it.
  const dirty = bufferIsDirty();
  // The latch is released when the buffer is GONE, not when it merely stops
  // being dirty: "Reload and discard" cleans the buffer without closing it,
  // and releasing there rebound the drawer to the bot the author had just
  // refused to follow — breaking the conflict banner's own promise.
  const bufferIsOpen = () => !!editing;
  const bufferIsOpenRef = useRef(bufferIsOpen);
  const buffered = bufferIsOpen();
  const bufferIsDirtyRef = useRef(bufferIsDirty);
  // F6: in an effect, not during render — the neighbouring `onSaveRef`
  // comment says why, and this ref's staleness is a silent buffer drop
  // rather than a stale save.
  useEffect(() => {
    bufferIsDirtyRef.current = bufferIsDirty;
    bufferIsOpenRef.current = bufferIsOpen;
  });

  /** The one gate on every exit that drops the typed buffer — closing the
   *  drawer, Back, and following the props to another bot. `go` runs when
   *  there is nothing to lose, or once the author has said so.
   *
   *  It must not close the drawer while the confirm is pending: the dialog
   *  renders inside this subtree, and `onOpenChange` is only a REQUEST —
   *  the parent owns `open` — so the close is withheld until the answer. */
  const leavingBuffer = async (go: () => void) => {
    if (bufferIsDirtyRef.current()) {
      const ok = await confirm({
        title: "Discard this text?",
        message:
          "You have not saved what you typed in this file. Leaving replaces it with the file as it is stored.",
        confirmLabel: "Discard",
        confirmVariant: "danger",
      });
      if (!ok) return;
    }
    go();
  };

  const handleOpenChange = (next: boolean) => {
    if (next) {
      onOpenChange(true);
      return;
    }
    void leavingBuffer(() => {
      setEditing(null);
      setConflict(false);
      onOpenChange(false);
    });
  };

  // Follow the props — after asking, when the buffer holds work for the
  // bundle being left. Declining keeps the drawer on the bundle the text
  // belongs to, which the title names, so the author can save it.
  useEffect(() => {
    if (!open) return;
    if (bound && bound.teamID === teamID && bound.slug === slug) return;
    const key = `${teamID}\u0000${slug}`;
    // The latch exists to stop the re-ask loop while there is something to
    // lose. Once the author has saved, there is nothing — and a drawer pinned
    // for ever would be divorced from the toolbar that opened it.
    if (declined.current === key && !bufferIsOpenRef.current()) declined.current = null;
    if (declined.current === key) return;
    let cancelled = false;
    let asked = false;
    void (async () => {
      if (bound && bufferIsDirtyRef.current()) {
        asked = true;
        const ok = await confirm({
          title: "Discard this text?",
          message:
            "The editor moved to another bot. You have not saved what you typed here — following it replaces your text with the other bot's files.",
          confirmLabel: "Discard",
          confirmVariant: "danger",
        });
        if (cancelled) return;
        if (!ok) {
          declined.current = key;
          return;
        }
      }
      if (cancelled) return;
      declined.current = null;
      setBound({ teamID, slug });
    })();
    return () => {
      cancelled = true;
      // A question this effect opened and can no longer answer must not stay
      // on screen: `useConfirm` keeps the dialog up until it is settled, and
      // a Radix modal aria-hides and pointer-blocks the whole app.
      if (asked) dismiss();
    };
  }, [open, teamID, slug, bound, confirm, dirty, buffered, dismiss]);

  // Load whatever the drawer is BOUND to.
  useEffect(() => {
    if (!open || !bound) return;
    let cancelled = false;
    // The buffer and the refusal belong to the bundle they were read from:
    // adopting another bundle's version under the old bot's text would write
    // that text into the new bot under a token the store has no reason to
    // refuse. The gate above is what makes this reset safe to do silently —
    // by the time it runs, the buffer holds nothing the author wants.
    setLoaded(null);
    setEditing(null);
    setConflict(false);
    setLoading(true);
    getBotSource(bound.teamID, bound.slug)
      .then((b) => {
        if (!cancelled) setBundle(b, bound);
      })
      .catch((err) => {
        if (!cancelled) toastError(addToast, err, "Load bundle failed");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, bound, addToast]);

  // A closed drawer forgets where it was, so reopening follows the props.
  useEffect(() => {
    if (open) return;
    setBound(null);
    declined.current = null;
  }, [open]);

  const paths = useMemo(() => {
    const keys = Object.keys(bundle?.files ?? {});
    // main.bot first, then manifest, then the rest alphabetically.
    return keys.sort((a, b) => {
      if (a === MAIN_BOT) return -1;
      if (b === MAIN_BOT) return 1;
      const am = a.startsWith("manifest.");
      const bm = b.startsWith("manifest.");
      if (am !== bm) return am ? -1 : 1;
      return a.localeCompare(b);
    });
  }, [bundle]);

  const openMainBot = () => {
    const file = botSourceEditorPath(shown.teamID, shown.slug, MAIN_BOT);
    useTabsStore.getState().openTab("editor", { file });
    setLocation(`/editor?file=${encodeURIComponent(file)}`);
    handleOpenChange(false);
  };

  const openFileForEdit = (rel: string) => {
    const content = bundle?.files?.[rel] ?? "";
    setEditing({ rel, value: content, original: content });
  };

  const onNewFile = async () => {
    // The pair this prompt was opened on. An open dialog is not a dirty
    // buffer, so the follow gate does not see it and the drawer can rebind
    // underneath — after which `bundle` names the bot we LEFT while every
    // write goes to the one we moved to. A path that exists in the new bot
    // and not in the old one then took the "create" branch and Save wrote an
    // empty file over a real one, under a token the store had every reason
    // to accept. Same rule as `setBundle`: an answer naming a pair we have
    // left is dropped, and said out loud rather than swallowed.
    const at = shownRef.current;
    const rel = await prompt({
      title: "New bundle file",
      label: "Relative path (e.g. skills/my-skill.md)",
      placeholder: "skills/my-skill.md",
      defaultValue: "skills/",
      confirmLabel: "Create",
      validate: (v) => {
        const clean = v.replace(/^\/+/, "");
        if (!clean || clean.endsWith("/")) return "Enter a file path";
        if (clean.includes("..")) return "Path may not contain '..'";
        return null;
      },
    });
    if (!rel) return;
    const now = shownRef.current;
    if (at.teamID !== now.teamID || at.slug !== now.slug) {
      addToast(
        `This panel moved to ${now.slug} while the dialog was open — "${rel}" was not created.`,
        "error",
        { persistent: true },
      );
      return;
    }
    const clean = rel.replace(/^\/+/, "");
    if (bundle?.files?.[clean] != null) {
      openFileForEdit(clean);
      return;
    }
    // `created` rather than a sentinel `original`: comparing value to a
    // sentinel made an untouched new file dirty from birth, so the discard
    // gate asked about text that does not exist.
    setEditing({ rel: clean, value: "", original: "", created: true });
  };

  // The if-match token for every write this drawer makes: the version of the
  // bundle it LOADED. Not a fresh read at write time — that would only cover
  // the milliseconds between the read and the PUT, and the window that loses
  // an author's work is the whole time the drawer has been open.
  const ifMatch = (): number | "unchecked" => bundle?.version ?? "unchecked";

  /** A write the store refused because the bundle moved under this drawer. */
  const isStale = (err: unknown) => err instanceof ApiError && err.status === 409;

  const onSaveRef = useRef<() => Promise<void>>(async () => {});
  // In an effect, not during render: a concurrent render React starts and
  // ABANDONS would still have written the ref, leaving the keybinding closed
  // over state that never committed.
  useEffect(() => {
    onSaveRef.current = onSave;
  });

  const onSave = async () => {
    if (!editing) return;
    setSaving(true);
    try {
      const updated = await putBotSourceFile(
        shown.teamID,
        shown.slug,
        editing.rel,
        editing.value,
        ifMatch(),
      );
      setBundle(updated, shown);
      setEditing(null);
      setConflict(false);
    } catch (err) {
      // The typed text stays in the buffer either way: the author's only
      // copy of it is on screen.
      if (isStale(err)) setConflict(true);
      toastError(addToast, err, "Save file failed");
    } finally {
      setSaving(false);
    }
  };

  /** Re-read the bundle, discarding what is in the buffer. The one way out
   *  of a conflict, and it says so before it takes the text. */
  const onReloadAfterConflict = async () => {
    const dirty = !!editing && editing.value !== editing.original;
    if (dirty) {
      const go = await confirm({
        title: "Reload this bot from the store?",
        message:
          "Reloading replaces what you typed with the file as it is stored now, including the change made by the other editor. Copy your text out first if you need it.",
        confirmLabel: "Reload and discard",
        confirmVariant: "danger",
      });
      if (!go) return;
    }
    setLoading(true);
    try {
      const fresh = await getBotSource(shown.teamID, shown.slug);
      setBundle(fresh, shown);
      setConflict(false);
      // Spread, not rebuild: `created` is part of the buffer's identity and
      // rebuilding dropped it, leaving Save disabled for ever on a file that
      // does not exist yet.
      setEditing((e) =>
        e
          ? { ...e, value: fresh.files?.[e.rel] ?? "", original: fresh.files?.[e.rel] ?? "" }
          : e,
      );
    } catch (err) {
      toastError(addToast, err, "Reload bundle failed");
    } finally {
      setLoading(false);
    }
  };

  const onDelete = async (rel: string) => {
    if (rel === MAIN_BOT) return;
    if (
      !(await confirm({
        title: "Delete file",
        message: `Delete ${rel} from this bot?`,
        confirmLabel: "Delete",
        confirmVariant: "danger",
      }))
    ) {
      return;
    }
    setBusyRel(rel);
    try {
      const updated = await deleteBotSourceFile(shown.teamID, shown.slug, rel, ifMatch());
      setBundle(updated, shown);
      setConflict(false);
    } catch (err) {
      if (isStale(err)) setConflict(true);
      toastError(addToast, err, "Delete file failed");
    } finally {
      setBusyRel(null);
    }
  };


  const monacoTheme = resolvedTheme === "dark" ? "vs-dark" : "vs";

  return (
    <>
      <Drawer
        open={open}
        onOpenChange={handleOpenChange}
        title={editing ? editing.rel : `Bundle files — ${shown.slug}`}
        description={
          editing
            ? "Editing a bundle file — saved to your team's bot store"
            : "The bot's workflow, skills and manifest"
        }
        widthClass={editing ? "max-w-[90vw] w-[90vw]" : "max-w-md"}
        footer={
          editing ? (
            <>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => void leavingBuffer(() => setEditing(null))}
                disabled={saving}
              >
                Back
              </Button>
              <Button
                variant="primary"
                size="sm"
                onClick={() => void onSave()}
                disabled={saving || (editing.value === editing.original && !editing.created)}
                loading={saving}
              >
                Save
              </Button>
            </>
          ) : (
            <Button variant="ghost" size="sm" onClick={() => handleOpenChange(false)}>
              Close
            </Button>
          )
        }
      >
        {conflict && (
          <InlineBanner
            tone="warning"
            layout="inline"
            title="This bot changed in the store"
            action={
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void onReloadAfterConflict()}
              >
                Reload
              </Button>
            }
          >
            Another editor wrote to it since this panel opened, so a write from
            here would overwrite that change. Reload to see it — what is typed
            here is discarded.
          </InlineBanner>
        )}
        {editing ? (
          <div className="-mx-4 -my-3 flex h-[75vh] flex-col">
            <Editor
              theme={monacoTheme}
              // A `.bot` fragment is iterion's own DSL, not plain text —
              // `inferMonacoLanguage` maps it to plaintext for the dialogs
              // that show a file without registering the language, so this
              // surface names the id and registers it in `beforeMount`.
              language={isWorkflowFile(editing.rel) ? ITER_LANGUAGE : inferMonacoLanguage(editing.rel)}
              beforeMount={registerIterLanguage}
              value={editing.value}
              onChange={(v) => setEditing((e) => (e ? { ...e, value: v ?? "" } : e))}
              onMount={(ed, monaco) => {
                // Through a ref: `@monaco-editor/react` captures `onMount`
                // at the editor's first render and calls it once, so a
                // handler bound here closes over the buffer and the version
                // as they were when it opened. Ctrl+S then wrote the file's
                // PRE-EDIT text, cleared the editor from that stale closure
                // — taking the author's typing with it — and kept presenting
                // the pre-reload token after a conflict.
                ed.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () =>
                  void onSaveRef.current(),
                );
              }}
              options={{
                automaticLayout: true,
                minimap: { enabled: false },
                scrollBeyondLastLine: false,
              }}
            />
          </div>
        ) : loading ? (
          <div className="flex flex-1 items-center justify-center py-8">
            <Spinner size="sm" label="Loading bundle" />
          </div>
        ) : !bundle ? (
          // No bundle means the read FAILED. The list then renders empty and
          // fully functional: every write goes out with no if-match token at
          // all, and "New file" typed with an existing path opens an empty
          // buffer that replaces a real file. Nothing here can be checked
          // against what is stored, so nothing here may write.
          <InlineBanner tone="danger" layout="inline" title="This bot could not be read">
            Its files are unavailable, so nothing written here could be checked against
            what is stored. Close and reopen this panel to try again.
          </InlineBanner>
        ) : (
          <div className="flex flex-col gap-1">
            <button
              type="button"
              onClick={() => void onNewFile()}
              className="mb-1 flex items-center gap-1.5 self-start rounded px-2 py-1 text-caption text-accent-text hover:bg-surface-2"
            >
              <PlusIcon className="h-3.5 w-3.5" /> New file
            </button>
            {paths.map((rel) => {
              const isMain = rel === MAIN_BOT;
              return (
                <div
                  key={rel}
                  {...referenceDragProps("bot-file", `${shown.teamID}/${shown.slug}/${rel}`, rel)}
                  className="group flex items-center gap-2 rounded px-2 py-1.5 hover:bg-surface-2"
                >
                  <FileIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />
                  <button
                    type="button"
                    onClick={() => (isMain ? openMainBot() : openFileForEdit(rel))}
                    className="min-w-0 flex-1 truncate text-left text-sm text-fg-default hover:text-accent-text"
                    title={isMain ? "Open the workflow in the Canvas editor" : `Edit ${rel}`}
                  >
                    {rel}
                    {isMain && <span className="ml-2 text-caption text-fg-subtle">workflow</span>}
                  </button>
                  {isMain ? (
                    // The canvas is the main's editor, but it REFUSES a main
                    // that does not parse: the document is then the file
                    // minus the region the parser could not read, and every
                    // write site turns it down. On the local twin an author
                    // repairs the file where it lives; on the cloud twin the
                    // bundle's files live here, so this is the only surface
                    // that can, and routing the row to the canvas alone left
                    // a stored bot unrepairable from the studio (#1659).
                    <button
                      type="button"
                      onClick={() => openFileForEdit(rel)}
                      className="shrink-0 rounded px-1.5 py-0.5 text-caption text-accent-text opacity-0 transition-opacity hover:bg-surface-2 group-hover:opacity-100"
                      title={`Edit ${rel} as text — the way to repair a main the canvas cannot open`}
                    >
                      Edit as text
                    </button>
                  ) : (
                    <button
                      type="button"
                      onClick={() => void onDelete(rel)}
                      disabled={busyRel === rel}
                      className="opacity-0 transition-opacity hover:text-danger group-hover:opacity-100"
                      title={`Delete ${rel}`}
                    >
                      <TrashIcon className="h-3.5 w-3.5" />
                    </button>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </Drawer>
      {confirmDialog}
      {promptDialog}
    </>
  );
}
