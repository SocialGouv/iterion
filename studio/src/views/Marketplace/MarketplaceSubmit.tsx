import { useId, useRef, useState } from "react";

import { uploadBotBundle } from "@/api/bots";
import type { MarketplaceScope } from "@/api/marketplace";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { useUIStore } from "@/store/ui";
import { toastError } from "@/lib/errorHints";

const SCOPE_LABELS: Record<MarketplaceScope, string> = {
  public: "Public — anyone can browse",
  instance: "Instance — any signed-in user",
  org: "Org — only my organization",
};

interface Props {
  onSubmit: (req: {
    repo_url: string;
    ref?: string;
    path?: string;
    tags?: string[];
    icon?: string;
    scope?: MarketplaceScope;
  }) => Promise<void>;
  /** Called after a successful .botz upload so the parent can re-reconcile
   *  the installed-state of the cards. */
  onUploaded?: () => void | Promise<void>;
  /** Visibility scopes the server allows (cloud). When more than one is
   *  offered a scope picker is shown; submissions then land for review. */
  scopes?: MarketplaceScope[];
  defaultScope?: MarketplaceScope;
  /** True when submissions land pending moderation (cloud). */
  moderated?: boolean;
  /** Cloud mode: the repo-URL submit path works, but the local `.botz`
   *  import pane is hidden — the server refuses bot uploads in cloud. */
  cloud?: boolean;
}

/** MarketplaceSubmit is the inline form for adding a repository to the
 *  hosted registry. The actual cloning/inspection happens server-side
 *  (botinstall.Inspect); we surface the validation result via the
 *  parent's toast. Collapsed by default to keep the browse list above
 *  the fold. */
export function MarketplaceSubmit({
  onSubmit,
  onUploaded,
  scopes,
  defaultScope,
  moderated,
  cloud = false,
}: Props) {
  const addToast = useUIStore((s) => s.addToast);
  const [expanded, setExpanded] = useState(false);
  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  const [path, setPath] = useState("");
  const [tagsRaw, setTagsRaw] = useState("");
  const [icon, setIcon] = useState("");
  const [scope, setScope] = useState<MarketplaceScope | undefined>(defaultScope);
  const [submitting, setSubmitting] = useState(false);
  const [uploading, setUploading] = useState(false);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const urlId = useId();
  const refId = useId();
  const pathId = useId();
  const tagsId = useId();
  const iconId = useId();
  const scopeId = useId();
  const showScopePicker = (scopes?.length ?? 0) > 1;

  const handleFile = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = ""; // allow re-selecting the same file
    if (!file) return;
    setUploading(true);
    try {
      const res = await uploadBotBundle(file, { force: true });
      addToast(`Imported ${res.name} → ${res.installed_path}`, "success");
      setExpanded(false);
      await onUploaded?.();
    } catch (err) {
      toastError(addToast, err, "Import failed");
    } finally {
      setUploading(false);
    }
  };

  const reset = () => {
    setUrl("");
    setRef("");
    setPath("");
    setTagsRaw("");
    setIcon("");
  };

  const handle = async () => {
    const repo = url.trim();
    if (!repo) return;
    setSubmitting(true);
    try {
      const tags = tagsRaw
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean);
      await onSubmit({
        repo_url: repo,
        ref: ref.trim() || undefined,
        path: path.trim() || undefined,
        tags: tags.length > 0 ? tags : undefined,
        icon: icon.trim() || undefined,
        scope: showScopePicker ? scope : undefined,
      });
      reset();
      setExpanded(false);
    } catch {
      // toast already raised by the parent
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <section className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)] p-4">
      <div className="flex items-center justify-between">
        <h2 className="text-xs font-medium text-fg-muted">Submit a repository</h2>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setExpanded((v) => !v)}
        >
          {expanded ? "Cancel" : "Submit…"}
        </Button>
      </div>
      {expanded && (
        <div className="mt-3 space-y-2">
          <div className="flex flex-col gap-1">
            <label
              htmlFor={urlId}
              className="text-caption uppercase tracking-wide text-fg-subtle"
            >
              Repository URL
            </label>
            <Input
              id={urlId}
              type="text"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="git URL (https://… or git@…) or local path"
            />
          </div>
          <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
            <div className="flex flex-col gap-1">
              <label
                htmlFor={refId}
                className="text-caption uppercase tracking-wide text-fg-subtle"
              >
                Ref (optional)
              </label>
              <Input
                id={refId}
                type="text"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                placeholder="branch or tag"
              />
            </div>
            <div className="flex flex-col gap-1">
              <label
                htmlFor={pathId}
                className="text-caption uppercase tracking-wide text-fg-subtle"
              >
                Subpath / bot name (optional)
              </label>
              <Input
                id={pathId}
                type="text"
                value={path}
                onChange={(e) => setPath(e.target.value)}
                placeholder="e.g. bots/featurly"
              />
            </div>
          </div>
          <div className="flex items-end gap-2">
            <div className="flex min-w-0 flex-1 flex-col gap-1">
              <label
                htmlFor={tagsId}
                className="text-caption uppercase tracking-wide text-fg-subtle"
              >
                Tags (comma-separated)
              </label>
              <Input
                id={tagsId}
                type="text"
                value={tagsRaw}
                onChange={(e) => setTagsRaw(e.target.value)}
                placeholder="review, kanban, sre"
              />
            </div>
            <div className="flex w-24 shrink-0 flex-col gap-1">
              <label
                htmlFor={iconId}
                className="text-caption uppercase tracking-wide text-fg-subtle"
              >
                Icon
              </label>
              <Input
                id={iconId}
                type="text"
                value={icon}
                onChange={(e) => setIcon(e.target.value)}
                placeholder="🤖"
                maxLength={4}
                aria-label="Icon (an emoji, 1-2 characters)"
                title="An emoji (1-2 characters)"
                className="text-center"
              />
            </div>
          </div>
          {showScopePicker && (
            <div className="flex flex-col gap-1">
              <label
                htmlFor={scopeId}
                className="text-caption uppercase tracking-wide text-fg-subtle"
              >
                Visibility
              </label>
              <Select
                id={scopeId}
                value={scope ?? ""}
                onChange={(e) => setScope(e.target.value as MarketplaceScope)}
              >
                {scopes?.map((sc) => (
                  <option key={sc} value={sc}>
                    {SCOPE_LABELS[sc] ?? sc}
                  </option>
                ))}
              </Select>
            </div>
          )}
          <div className="flex items-center justify-between gap-3">
            <p className="text-caption text-fg-subtle">
              The server clones the repo and validates it (no install). Bot
              bundles and plugin.yaml repos are detected automatically.
              {moderated
                ? " Your submission is queued for an admin to review before it appears."
                : " Submitting again with the same name refreshes the entry."}
            </p>
            <Button
              variant="success"
              size="sm"
              className="shrink-0"
              onClick={() => void handle()}
              disabled={submitting || !url.trim()}
              loading={submitting}
            >
              {submitting ? "Submitting…" : "Submit"}
            </Button>
          </div>

          {!cloud && (
            <>
              <div className="flex items-center gap-3 pt-1">
                <div className="h-px flex-1 bg-border-default" />
                <span className="text-caption uppercase tracking-wide text-fg-subtle">or</span>
                <div className="h-px flex-1 bg-border-default" />
              </div>
              <div className="flex items-center justify-between gap-3">
                <p className="text-caption text-fg-subtle">
                  Import a packaged <code className="text-fg-default">.botz</code> bundle
                  directly into this workspace's <code className="text-fg-default">.botz/</code>.
                </p>
                <input
                  ref={fileRef}
                  type="file"
                  accept=".botz"
                  className="hidden"
                  onChange={(e) => void handleFile(e)}
                />
                <Button
                  variant="secondary"
                  size="sm"
                  className="shrink-0"
                  onClick={() => fileRef.current?.click()}
                  disabled={uploading}
                  loading={uploading}
                >
                  {uploading ? "Importing…" : "Import .botz file"}
                </Button>
              </div>
            </>
          )}
        </div>
      )}
    </section>
  );
}
