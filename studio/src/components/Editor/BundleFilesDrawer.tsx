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
export default function BundleFilesDrawer({ teamID, slug, open, onOpenChange }: Props) {
  const [, setLocation] = useLocation();
  const addToast = useUIStore((s) => s.addToast);
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const { confirm, dialog: confirmDialog } = useConfirm();
  const { prompt, dialog: promptDialog } = usePromptText();

  const [bundle, setBundle] = useState<BotSourceFull | null>(null);
  const [loading, setLoading] = useState(false);
  const [editing, setEditing] = useState<{ rel: string; value: string; original: string } | null>(
    null,
  );
  const [saving, setSaving] = useState(false);
  const [busyRel, setBusyRel] = useState<string | null>(null);
  // Set when the store refused a write because the bundle moved under this
  // drawer. It is NOT cleared by re-fetching in the background: the token
  // this drawer holds is the one it READ, and silently adopting a fresh one
  // would turn the refusal into the overwrite it just prevented. It is
  // cleared by an explicit reload, which says what it discards, and by the
  // load effect, which re-reads the bundle in the same breath.
  const [conflict, setConflict] = useState(false);

  const handleOpenChange = (next: boolean) => {
    if (!next) {
      setEditing(null);
      setConflict(false);
    }
    onOpenChange(next);
  };

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    // The buffer and the refusal belong to the bundle they were read from.
    // The drawer's props follow the active editor file, so they can move to
    // ANOTHER bot while it is open — and adopting the new bundle's version
    // under the old bot's text writes that text into the new bot under a
    // token the store has no reason to refuse.
    setBundle(null);
    setEditing(null);
    setConflict(false);
    setLoading(true);
    getBotSource(teamID, slug)
      .then((b) => {
        if (!cancelled) setBundle(b);
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
  }, [open, teamID, slug, addToast]);

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
    const file = botSourceEditorPath(teamID, slug, MAIN_BOT);
    useTabsStore.getState().openTab("editor", { file });
    setLocation(`/editor?file=${encodeURIComponent(file)}`);
    handleOpenChange(false);
  };

  const openFileForEdit = (rel: string) => {
    const content = bundle?.files?.[rel] ?? "";
    setEditing({ rel, value: content, original: content });
  };

  const onNewFile = async () => {
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
    const clean = rel.replace(/^\/+/, "");
    if (bundle?.files?.[clean] != null) {
      openFileForEdit(clean);
      return;
    }
    setEditing({ rel: clean, value: "", original: " new" });
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
        teamID,
        slug,
        editing.rel,
        editing.value,
        ifMatch(),
      );
      setBundle(updated);
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
      const fresh = await getBotSource(teamID, slug);
      setBundle(fresh);
      setConflict(false);
      setEditing((e) =>
        e ? { rel: e.rel, value: fresh.files?.[e.rel] ?? "", original: fresh.files?.[e.rel] ?? "" } : e,
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
      const updated = await deleteBotSourceFile(teamID, slug, rel, ifMatch());
      setBundle(updated);
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
        title={editing ? editing.rel : `Bundle files — ${slug}`}
        description={
          editing
            ? "Editing a bundle file — saved to your team's bot store"
            : "The bot's workflow, skills and manifest"
        }
        widthClass={editing ? "max-w-[90vw] w-[90vw]" : "max-w-md"}
        footer={
          editing ? (
            <>
              <Button variant="ghost" size="sm" onClick={() => setEditing(null)} disabled={saving}>
                Back
              </Button>
              <Button
                variant="primary"
                size="sm"
                onClick={() => void onSave()}
                disabled={saving || editing.value === editing.original}
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
                  {...referenceDragProps("bot-file", `${teamID}/${slug}/${rel}`, rel)}
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
