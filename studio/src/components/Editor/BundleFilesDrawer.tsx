import { useEffect, useMemo, useState } from "react";
import Editor from "@monaco-editor/react";
import { FileIcon, PlusIcon, TrashIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import {
  deleteBotSourceFile,
  getBotSource,
  putBotSourceFile,
  type BotSourceFull,
} from "@/api/botSources";
import { botSourceEditorPath } from "@/api/client";
import { Button, Drawer, Spinner } from "@/components/ui";
import { useConfirm } from "@/hooks/useConfirm";
import { usePromptText } from "@/hooks/usePromptText";
import { inferMonacoLanguage } from "@/lib/inferMonacoLanguage";
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

  const handleOpenChange = (next: boolean) => {
    if (!next) setEditing(null);
    onOpenChange(next);
  };

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
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

  const onSave = async () => {
    if (!editing) return;
    setSaving(true);
    try {
      const updated = await putBotSourceFile(teamID, slug, editing.rel, editing.value);
      setBundle(updated);
      setEditing(null);
    } catch (err) {
      toastError(addToast, err, "Save file failed");
    } finally {
      setSaving(false);
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
      const updated = await deleteBotSourceFile(teamID, slug, rel);
      setBundle(updated);
    } catch (err) {
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
        {editing ? (
          <div className="-mx-4 -my-3 flex h-[75vh] flex-col">
            <Editor
              theme={monacoTheme}
              language={inferMonacoLanguage(editing.rel)}
              value={editing.value}
              onChange={(v) => setEditing((e) => (e ? { ...e, value: v ?? "" } : e))}
              onMount={(ed, monaco) => {
                ed.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => void onSave());
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
                  {!isMain && (
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
