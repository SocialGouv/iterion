/**
 * ConfigShareView — the shell-less, self-authenticating editor at /config/:id.
 *
 * Purpose: a non-operator opens a bookmarkable link and edits ONLY the
 * feeds[] + editorial fields of ONE category of a veille config. Nothing
 * from the studio (Sidebar, Header, AppShell, cookie session) leaks in.
 *
 * Security posture — enforced here + by the isolated fetch client at
 * @/api/configShare + by an eslint no-restricted-imports rule:
 *   1. NO import of @/api/client. Every network call goes through
 *      getShareMeta / getShareConfig / patchShareConfig, which set
 *      credentials: "omit" and send the share token ONLY as Bearer.
 *   2. Token is read from window.location.hash (or ?token=…) at first
 *      mount, then immediately stripped from the visible URL via
 *      history.replaceState so a browser-tab screenshot / paste doesn't
 *      leak it. It's held in memory + sessionStorage (per-tab, cleared on
 *      close) — never localStorage.
 *   3. editorial is rendered as a plain <textarea>. No markdown preview,
 *      no dangerouslySetInnerHTML. Feed URLs are plain <input type=url>,
 *      never rendered as clickable <a> until the operator side re-reads
 *      them from the file.
 *   4. On 409 conflict, the fresh server projection + the user's draft are
 *      shown side-by-side and require an explicit "overwrite" click. The
 *      view NEVER auto-retries a PATCH.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "wouter";

import {
  getShareConfig,
  getShareMeta,
  patchShareConfig,
  ShareApiError,
  type ShareMeta,
} from "@/api/configShare";
import { BrandMark } from "@/components/ui/BrandMark";
import { BrandWordmark } from "@/components/ui/BrandWordmark";
import { Button } from "@/components/ui/Button";
import { Card } from "@/components/ui/Card";
import { Dialog } from "@/components/ui/Dialog";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Spinner } from "@/components/ui/Spinner";
import { Textarea } from "@/components/ui/Textarea";
import { ThemeToggle } from "@/components/ui/ThemeToggle";

// TOKEN_SESSION_KEY_PREFIX namespaces the sessionStorage entry so the same
// tab can hold tokens for different shares (rare, but cheap to support).
const TOKEN_SESSION_KEY_PREFIX = "iterion.config-share.token.";

// httpUrlPattern is the client-side echo of the server's URL check. The
// server is the authority; this only avoids a round-trip for obvious typos.
const httpUrlPattern = /^https?:\/\/\S+$/i;

// extractInitialToken reads the token from the URL hash (preferred; the
// operator's "copy link" format lives there) or ?token=… (fallback for a
// paste-from-QR / stored bookmark). Strips the found token from the visible
// URL via history.replaceState so a screenshot doesn't leak it.
function extractInitialToken(shareID: string): string | null {
  if (typeof window === "undefined") return null;
  const stored = window.sessionStorage.getItem(TOKEN_SESSION_KEY_PREFIX + shareID);
  const hash = window.location.hash.replace(/^#/, "");
  const query = new URLSearchParams(window.location.search);
  const fromQuery = query.get("token") ?? "";
  const fromHash = hash;
  const raw = fromHash || fromQuery || stored || "";
  // Only accept the iws_ share-token prefix. Anything else (a random hash
  // fragment, a stray query string) is ignored — the user sees "link needs
  // its token".
  const cleaned = raw.trim();
  if (!cleaned.startsWith("iws_")) {
    return stored?.startsWith("iws_") ? stored : null;
  }
  // Strip from URL bar. keep the pathname; drop hash and any ?token=.
  if (typeof window.history?.replaceState === "function") {
    query.delete("token");
    const cleanSearch = query.toString();
    const nextURL = window.location.pathname + (cleanSearch ? "?" + cleanSearch : "");
    window.history.replaceState({}, "", nextURL);
  }
  try {
    window.sessionStorage.setItem(TOKEN_SESSION_KEY_PREFIX + shareID, cleaned);
  } catch {
    // sessionStorage denied (private mode, cross-origin) — memory is enough.
  }
  return cleaned;
}

// getVisiblePathValue reads a dotted path (e.g. "categories.a11y.feeds") out
// of the nested projection returned by GET /config-share/:id/config.
// Returns undefined when the path is missing.
function getPathValue(obj: Record<string, unknown>, path: string): unknown {
  let cur: unknown = obj;
  for (const p of path.split(".")) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[p];
  }
  return cur;
}

// setPathValue writes into a nested object, creating intermediate branches
// as needed. Used to construct the sparse PATCH body.
function setPathValue(target: Record<string, unknown>, path: string, value: unknown): void {
  const parts = path.split(".");
  if (parts.length === 0) return;
  let cur: Record<string, unknown> = target;
  for (let i = 0; i < parts.length - 1; i++) {
    const p = parts[i]!;
    const next = cur[p];
    if (next === null || typeof next !== "object" || Array.isArray(next)) {
      cur[p] = {};
    }
    cur = cur[p] as Record<string, unknown>;
  }
  cur[parts[parts.length - 1]!] = value;
}

// draftKey turns an allowed-path into a UI form key — we drive one input per
// allowed leaf, keyed by the last segment (e.g. "feeds", "editorial").
function pathLeaf(path: string): string {
  const parts = path.split(".");
  return parts.length > 0 ? parts[parts.length - 1]! : path;
}

// isFeedsPath / isEditorialPath classify an allowed_paths entry. The
// server-side share is scoped to ONE category and lists the leaves it
// allows; we render a dedicated widget per field type.
function isFeedsPath(path: string): boolean {
  return pathLeaf(path) === "feeds";
}
function isEditorialPath(path: string): boolean {
  return pathLeaf(path) === "editorial";
}

// ShellHeader is the slim shell-less top bar. Kept intentionally minimal —
// no navigation, no team switcher, no user chip — because the visitor is
// NOT an operator. Just the brand + a theme toggle.
function ShellHeader({ label }: { label: string }) {
  return (
    <header className="sticky top-0 z-10 flex items-center justify-between border-b border-border-subtle bg-surface-0/90 px-4 py-3 backdrop-blur sm:px-6">
      <div className="flex min-w-0 items-center gap-2.5">
        <BrandMark className="h-6 w-6 shrink-0" />
        <BrandWordmark />
        <span aria-hidden className="text-fg-subtle">
          /
        </span>
        <span className="truncate text-sm font-medium text-fg-default">{label}</span>
      </div>
      <ThemeToggle />
    </header>
  );
}

// ShellFrame is the shared page chrome: the ShellHeader plus a centered
// content column. Every top-level state (loading, error, ready) renders
// inside this so the layout doesn't jump.
function ShellFrame({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="min-h-screen bg-surface-0 text-fg-default">
      <ShellHeader label={label} />
      <main className="mx-auto w-full max-w-3xl px-4 py-6 sm:px-6">{children}</main>
    </div>
  );
}

// ScopeNote is the standing anti-injection notice: reinforces that this is
// a scoped editor, describes what the operator granted, and reminds the
// visitor to keep the link private.
function ScopeNote({ meta }: { meta: ShareMeta }) {
  return (
    <InlineBanner
      tone="info"
      layout="inline"
      title={`Scoped editor — ${meta.label || meta.bot_id}`}
    >
      You are editing category <strong>{meta.category}</strong> of{" "}
      <code className="font-mono">{meta.config_path}</code>. Only the fields listed
      below are editable — everything else in the file is out of scope.
      {" "}Treat this link + its token as a shared password: it grants write access
      until it is revoked.
    </InlineBanner>
  );
}

// ---------------------------------------------------------------------------
// Route entry — resolves the :id, extracts the token, dispatches by state.
// ---------------------------------------------------------------------------

export default function ConfigShareView() {
  const params = useParams<{ id: string }>();
  const shareID = decodeURIComponent(params.id ?? "");
  const [token, setToken] = useState<string | null>(() => extractInitialToken(shareID));

  useEffect(() => {
    // If the URL later re-adds a token (some browsers restore #fragment on
    // history nav), re-run the strip pass. Cheap; only touches the URL when
    // there's actually a hash to strip.
    if (!token) {
      const fromURL = extractInitialToken(shareID);
      if (fromURL) setToken(fromURL);
    }
  }, [shareID, token]);

  if (!shareID) {
    return (
      <ShellFrame label="Config editor">
        <InlineBanner tone="danger" layout="inline" title="Invalid link">
          The share id is missing from the URL.
        </InlineBanner>
      </ShellFrame>
    );
  }
  if (!token) {
    return (
      <ShellFrame label="Config editor">
        <InlineBanner tone="warning" layout="inline" title="This link needs its token">
          The URL you followed is missing its <code className="font-mono">#iws_…</code>{" "}
          token. Ask whoever shared it for the complete link (the part after the{" "}
          <code className="font-mono">#</code> is part of the credential — some chat
          apps strip it).
        </InlineBanner>
      </ShellFrame>
    );
  }
  return <Editor shareID={shareID} token={token} />;
}

// ---------------------------------------------------------------------------
// Editor — meta + config load, form state, save, conflict resolution.
// ---------------------------------------------------------------------------

interface DraftState {
  feeds: string[];
  editorial: string;
}

// initialDraft projects the fetched config into the shape the form drives.
// Feeds default to a single empty row so a first-time editor sees a field
// to type in (empty array otherwise would render nothing).
function initialDraft(
  meta: ShareMeta,
  config: Record<string, unknown>,
): DraftState {
  const feedsPath = meta.allowed_paths.find(isFeedsPath);
  const editorialPath = meta.allowed_paths.find(isEditorialPath);
  const feedsRaw = feedsPath ? getPathValue(config, feedsPath) : undefined;
  const editorialRaw = editorialPath ? getPathValue(config, editorialPath) : undefined;
  return {
    feeds: Array.isArray(feedsRaw)
      ? feedsRaw.map((v) => (typeof v === "string" ? v : String(v ?? "")))
      : [""],
    editorial: typeof editorialRaw === "string" ? editorialRaw : "",
  };
}

// buildPatch turns the current draft back into a sparse nested-object PATCH
// carrying ONLY the leaves the share allows.
function buildPatch(meta: ShareMeta, draft: DraftState): Record<string, unknown> {
  const patch: Record<string, unknown> = {};
  for (const path of meta.allowed_paths) {
    if (isFeedsPath(path)) {
      // Drop empty rows on submit — a blank feed URL is a UX aid, not data.
      setPathValue(
        patch,
        path,
        draft.feeds.map((f) => f.trim()).filter((f) => f.length > 0),
      );
      continue;
    }
    if (isEditorialPath(path)) {
      setPathValue(patch, path, draft.editorial);
      continue;
    }
    // Unknown allowed_paths from a future server: skip rather than fabricate.
    // The operator side would need to teach this view about a new leaf; we'd
    // rather send nothing than a wrong-shape value.
  }
  return patch;
}

// hasDraftChanges reports whether the current form state differs from the
// server-side baseline. Powers the disabled state of the Save button.
function hasDraftChanges(baseline: DraftState, draft: DraftState): boolean {
  if (baseline.editorial !== draft.editorial) return true;
  const a = baseline.feeds.map((f) => f.trim()).filter(Boolean);
  const b = draft.feeds.map((f) => f.trim()).filter(Boolean);
  if (a.length !== b.length) return true;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return true;
  return false;
}

// visibleTitleFor returns the digest_title (or first found visible-only
// string leaf under categories.<cat>) so the editor shows the operator's
// category label for context, without letting the visitor edit it.
function visibleContextTitle(meta: ShareMeta, config: Record<string, unknown>): string | null {
  const allowedSet = new Set(meta.allowed_paths);
  for (const path of meta.visible_paths) {
    if (allowedSet.has(path)) continue;
    const v = getPathValue(config, path);
    if (typeof v === "string" && v.trim().length > 0 && path.endsWith("digest_title")) {
      return v;
    }
  }
  return null;
}

function Editor({ shareID, token }: { shareID: string; token: string }) {
  const [meta, setMeta] = useState<ShareMeta | null>(null);
  const [baseline, setBaseline] = useState<DraftState | null>(null);
  const [draft, setDraft] = useState<DraftState | null>(null);
  const [contextTitle, setContextTitle] = useState<string | null>(null);
  const [sha, setSha] = useState<string>("");
  const [loadError, setLoadError] = useState<{
    kind: "unauthorized" | "not_found" | "network";
    message: string;
  } | null>(null);
  const [saving, setSaving] = useState(false);
  // Save status: "idle" resets on any input; "saved" fades after a moment.
  const [saveStatus, setSaveStatus] = useState<
    | { kind: "idle" }
    | { kind: "saved"; changed: string[] }
    | { kind: "error"; message: string }
  >({ kind: "idle" });
  // Conflict state carries the server's fresh projection so the modal can
  // render "yours vs theirs" and require an explicit overwrite.
  const [conflict, setConflict] = useState<{
    serverConfig: Record<string, unknown>;
    serverSha: string;
    serverDraft: DraftState;
  } | null>(null);
  // Guard StrictMode-double-invocation of the boot load.
  const bootRef = useRef(false);

  const clearSaveStatus = useCallback(() => setSaveStatus({ kind: "idle" }), []);

  const bootstrap = useCallback(async () => {
    setLoadError(null);
    try {
      // Fetch meta first so the shell header can render the label as soon as
      // the config load starts.
      const m = await getShareMeta(shareID, token);
      setMeta(m);
      const cfg = await getShareConfig(shareID, token);
      const d = initialDraft(m, cfg.config);
      setBaseline(d);
      setDraft(d);
      setSha(cfg.sha);
      setContextTitle(visibleContextTitle(m, cfg.config));
    } catch (err) {
      if (err instanceof ShareApiError) {
        if (err.status === 401) {
          setLoadError({ kind: "unauthorized", message: err.message });
        } else if (err.status === 404) {
          setLoadError({ kind: "not_found", message: err.message });
        } else {
          setLoadError({ kind: "network", message: err.message });
        }
        return;
      }
      setLoadError({
        kind: "network",
        message: err instanceof Error ? err.message : String(err),
      });
    }
  }, [shareID, token]);

  useEffect(() => {
    if (bootRef.current) return;
    bootRef.current = true;
    void bootstrap();
  }, [bootstrap]);

  const label = meta?.label?.trim() || meta?.bot_id || "Config editor";

  if (loadError) {
    return (
      <ShellFrame label={label}>
        {loadError.kind === "unauthorized" ? (
          <InlineBanner tone="danger" layout="inline" title="Link expired or revoked">
            The server rejected this share token. Ask the operator to send a fresh link.
          </InlineBanner>
        ) : loadError.kind === "not_found" ? (
          <InlineBanner tone="danger" layout="inline" title="Share not found">
            No share with this id is registered on this server.
          </InlineBanner>
        ) : (
          <InlineBanner tone="danger" layout="inline" title="Couldn't load the config">
            {loadError.message}
            <div className="mt-2">
              <Button variant="secondary" size="sm" onClick={() => void bootstrap()}>
                Try again
              </Button>
            </div>
          </InlineBanner>
        )}
      </ShellFrame>
    );
  }

  if (!meta || !draft || !baseline) {
    return (
      <ShellFrame label={label}>
        <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
          <Spinner /> Loading configuration…
        </div>
      </ShellFrame>
    );
  }

  const readOnly = meta.read_only;
  const dirty = hasDraftChanges(baseline, draft);

  const onSave = async (overwriteFromConflict?: {
    forcedSha: string;
    forcedServerDraft: DraftState;
  }) => {
    if (readOnly) return;
    setSaving(true);
    setSaveStatus({ kind: "idle" });
    try {
      const patch = buildPatch(meta, draft);
      const useSha = overwriteFromConflict?.forcedSha ?? sha;
      const result = await patchShareConfig(shareID, token, patch, useSha);
      if (result.kind === "conflict") {
        const serverDraft = initialDraft(meta, result.config);
        setConflict({
          serverConfig: result.config,
          serverSha: result.sha,
          serverDraft,
        });
        setSaveStatus({
          kind: "error",
          message: "Someone else edited the file — review the differences before saving.",
        });
        return;
      }
      // Success: rebase our baseline on what we just wrote so a follow-up
      // save doesn't re-send the same fields as "changed".
      setBaseline({ ...draft });
      setSha(result.sha);
      setSaveStatus({ kind: "saved", changed: result.changed });
      setConflict(null);
    } catch (err) {
      if (err instanceof ShareApiError && err.status === 401) {
        setLoadError({
          kind: "unauthorized",
          message: "Session expired — the share may have been rotated or revoked.",
        });
        return;
      }
      setSaveStatus({
        kind: "error",
        message: err instanceof Error ? err.message : String(err),
      });
    } finally {
      setSaving(false);
    }
  };

  return (
    <ShellFrame label={label}>
      <div className="flex flex-col gap-3">
        <ScopeNote meta={meta} />
        {contextTitle && (
          <div className="rounded-md border border-border-default bg-surface-2 px-3 py-2 text-xs text-fg-muted">
            Category label:{" "}
            <span className="font-medium text-fg-default">{contextTitle}</span>
          </div>
        )}
        {readOnly && (
          <InlineBanner tone="info" layout="inline" title="Read-only">
            The operator granted read-only access. You can review the current values
            below but the Save button is disabled.
          </InlineBanner>
        )}
        <FeedsEditor
          feeds={draft.feeds}
          disabled={readOnly}
          onChange={(feeds) => {
            setDraft({ ...draft, feeds });
            clearSaveStatus();
          }}
        />
        <EditorialEditor
          value={draft.editorial}
          disabled={readOnly}
          onChange={(editorial) => {
            setDraft({ ...draft, editorial });
            clearSaveStatus();
          }}
        />

        <div className="sticky bottom-0 -mx-4 border-t border-border-subtle bg-surface-0/95 px-4 py-3 backdrop-blur sm:-mx-6 sm:px-6">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <StatusLine status={saveStatus} readOnly={readOnly} dirty={dirty} />
            <div className="flex items-center gap-2">
              <Button
                variant="ghost"
                size="sm"
                disabled={saving || !dirty || readOnly}
                onClick={() => {
                  setDraft(baseline);
                  clearSaveStatus();
                }}
              >
                Reset
              </Button>
              <Button
                variant="primary"
                size="md"
                loading={saving}
                disabled={saving || !dirty || readOnly}
                onClick={() => void onSave()}
              >
                Save changes
              </Button>
            </div>
          </div>
        </div>
      </div>

      {conflict && (
        <ConflictDialog
          yours={draft}
          server={conflict.serverDraft}
          onCancel={() => setConflict(null)}
          onOverwrite={() =>
            void onSave({
              forcedSha: conflict.serverSha,
              forcedServerDraft: conflict.serverDraft,
            })
          }
          onAdoptServer={() => {
            setDraft(conflict.serverDraft);
            setBaseline(conflict.serverDraft);
            setSha(conflict.serverSha);
            setConflict(null);
            setSaveStatus({ kind: "idle" });
          }}
        />
      )}
    </ShellFrame>
  );
}

// ---------------------------------------------------------------------------
// Field widgets
// ---------------------------------------------------------------------------

function FeedsEditor({
  feeds,
  disabled,
  onChange,
}: {
  feeds: string[];
  disabled: boolean;
  onChange: (feeds: string[]) => void;
}) {
  const setAt = (i: number, v: string) => {
    const next = feeds.slice();
    next[i] = v;
    onChange(next);
  };
  const removeAt = (i: number) => {
    const next = feeds.slice();
    next.splice(i, 1);
    if (next.length === 0) next.push("");
    onChange(next);
  };
  const addRow = () => onChange([...feeds, ""]);

  return (
    <Card>
      <div className="mb-2 flex items-baseline justify-between">
        <FieldLabel help="One RSS/Atom URL per row. The server rejects anything that isn't http(s).">
          Feeds
        </FieldLabel>
        <span className="text-caption text-fg-subtle">
          {feeds.filter((f) => f.trim().length > 0).length} source
          {feeds.filter((f) => f.trim().length > 0).length === 1 ? "" : "s"}
        </span>
      </div>
      <ul className="flex flex-col gap-2">
        {feeds.map((url, i) => {
          const trimmed = url.trim();
          const err = trimmed.length > 0 && !httpUrlPattern.test(trimmed);
          return (
            <li key={i} className="flex items-center gap-2">
              <Input
                type="url"
                inputMode="url"
                autoComplete="off"
                spellCheck={false}
                size="md"
                value={url}
                error={err}
                disabled={disabled}
                placeholder="https://example.org/feed.xml"
                aria-label={`Feed URL ${i + 1}`}
                onChange={(e) => setAt(i, e.target.value)}
                className="font-mono"
              />
              <Button
                variant="ghost"
                size="sm"
                disabled={disabled}
                onClick={() => removeAt(i)}
                aria-label={`Remove feed ${i + 1}`}
              >
                Remove
              </Button>
            </li>
          );
        })}
      </ul>
      <div className="mt-2">
        <Button variant="secondary" size="sm" disabled={disabled} onClick={addRow}>
          + Add feed
        </Button>
      </div>
    </Card>
  );
}

function EditorialEditor({
  value,
  disabled,
  onChange,
}: {
  value: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  return (
    <Card>
      <FieldLabel help="Free-text prompt injected into the digest. Rendered as-is (no markdown preview here).">
        Editorial prompt
      </FieldLabel>
      <Textarea
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        rows={8}
        placeholder="Ex: privilégier les sources gouvernementales, résumer en 3 lignes maximum, …"
        className="font-mono"
      />
    </Card>
  );
}

function StatusLine({
  status,
  readOnly,
  dirty,
}: {
  status:
    | { kind: "idle" }
    | { kind: "saved"; changed: string[] }
    | { kind: "error"; message: string };
  readOnly: boolean;
  dirty: boolean;
}) {
  if (readOnly) return <span className="text-xs text-fg-subtle">Read-only share</span>;
  if (status.kind === "saved") {
    return (
      <span className="text-xs text-success-fg">
        Saved · {status.changed.length} field{status.changed.length === 1 ? "" : "s"} updated
      </span>
    );
  }
  if (status.kind === "error") {
    return <span className="text-xs text-danger-fg">{status.message}</span>;
  }
  return (
    <span className="text-xs text-fg-subtle">
      {dirty ? "Unsaved changes" : "No changes"}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Conflict resolution — explicit user action, never a silent retry.
// ---------------------------------------------------------------------------

function ConflictDialog({
  yours,
  server,
  onCancel,
  onOverwrite,
  onAdoptServer,
}: {
  yours: DraftState;
  server: DraftState;
  onCancel: () => void;
  onOverwrite: () => void;
  onAdoptServer: () => void;
}) {
  const yoursFeeds = useMemo(() => yours.feeds.filter((f) => f.trim()), [yours.feeds]);
  const serverFeeds = useMemo(() => server.feeds.filter((f) => f.trim()), [server.feeds]);
  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onCancel();
      }}
      title="This config changed on the server"
      description="Someone else edited the file after you opened it. Review the differences below and choose how to proceed — no automatic retry."
      widthClass="max-w-3xl"
      stack="confirm"
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onCancel}>
            Keep editing
          </Button>
          <Button variant="secondary" size="sm" onClick={onAdoptServer}>
            Use the server version
          </Button>
          <Button variant="danger" size="sm" onClick={onOverwrite}>
            Overwrite with mine
          </Button>
        </>
      }
    >
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <ConflictPane title="Your draft" feeds={yoursFeeds} editorial={yours.editorial} />
        <ConflictPane
          title="Server version (current)"
          feeds={serverFeeds}
          editorial={server.editorial}
          highlight
        />
      </div>
    </Dialog>
  );
}

function ConflictPane({
  title,
  feeds,
  editorial,
  highlight = false,
}: {
  title: string;
  feeds: string[];
  editorial: string;
  highlight?: boolean;
}) {
  return (
    <div
      className={`rounded-md border p-3 text-xs ${
        highlight ? "border-accent bg-accent-soft/50" : "border-border-default bg-surface-2"
      }`}
    >
      <h3 className="mb-2 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
        {title}
      </h3>
      <div className="mb-2">
        <div className="text-caption text-fg-subtle">Feeds ({feeds.length})</div>
        {feeds.length === 0 ? (
          <p className="text-fg-subtle italic">empty</p>
        ) : (
          <ul className="mt-1 space-y-0.5 font-mono">
            {feeds.map((f, i) => (
              <li key={i} className="truncate">
                {f}
              </li>
            ))}
          </ul>
        )}
      </div>
      <div>
        <div className="text-caption text-fg-subtle">Editorial</div>
        <pre className="mt-1 whitespace-pre-wrap wrap-break-word font-mono text-fg-default">
          {editorial || <span className="italic text-fg-subtle">empty</span>}
        </pre>
      </div>
    </div>
  );
}
