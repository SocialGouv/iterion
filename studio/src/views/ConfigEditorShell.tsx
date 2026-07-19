/**
 * ConfigEditorShell — the limited studio surface for the least-privilege
 * `config_editor` team role.
 *
 * A config_editor is a real, cookie-authenticated team member who may do
 * exactly ONE thing: edit their team's config-shares. They get no Sidebar, no
 * runs/board/launch — just this shell (modeled on RestrictedShell): the brand
 * header + a config-editor workspace.
 *
 * The network layer is @/api/configEditor, which rides the SHARED cookie
 * client (credentials: "include"). This is deliberately NOT the isolated
 * @/api/configShare (iws_-token, credentials:"omit") used by the anonymous
 * /config/:id editor — that boundary is eslint-enforced under
 * src/views/ConfigShare/**; this signed-in shell lives outside it.
 *
 * The field-editing UX mirrors the proven ConfigShare editor: a plain
 * <textarea> for string fields (no markdown preview), a text-input list for
 * array fields (feeds), Save that sends {sha, patch}, and a 409 conflict flow
 * that shows "yours vs theirs" and NEVER auto-clobbers. Unlike ConfigShare
 * (hard-wired to feeds+editorial), this shell renders whatever editable string
 * / string-array leaves the server's projected config contains.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { useAuth } from "@/auth/AuthContext";
import { errorMessage } from "@/lib/errorHints";
import {
  getEditorConfig,
  getEditorSchedule,
  listEditorShares,
  patchEditorConfig,
  patchEditorSchedule,
  type EditorConfigResponse,
  type EditorShare,
} from "@/api/configEditor";
import { BrandMark } from "@/components/ui/BrandMark";
import { ThemeToggle } from "@/components/ui/ThemeToggle";
import {
  BrandWordmark,
  Button,
  Card,
  Dialog,
  EmptyState,
  FieldLabel,
  InlineBanner,
  Input,
  Spinner,
  Textarea,
} from "@/components/ui";

// ---------------------------------------------------------------------------
// Editable-field model — walk the server's projected config into a flat list
// of editable string / string-array leaves.
// ---------------------------------------------------------------------------

type FieldKind = "string" | "array";
type FieldValue = string | string[];

interface EditableField {
  /** Dotted path from the config root, e.g. "categories.a11y.feeds". */
  path: string;
  /** Last path segment, used as the field label ("feeds", "editorial"). */
  leaf: string;
  /** Dotted parent path ("categories › a11y"), "" for a top-level leaf. */
  parentLabel: string;
  kind: FieldKind;
}

const httpUrlPattern = /^https?:\/\/\S+$/i;

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every((x) => typeof x === "string");
}

// walkEditableFields does a stable depth-first walk of the projection,
// collecting every string leaf and every (all-string) array leaf. Nested
// objects are recursed; numbers/booleans/null and arrays-of-objects are
// skipped (nothing to render an input for). The server has already projected
// `config` down to the editable surface, so in practice every leaf here is
// something the PATCH accepts.
function walkEditableFields(
  config: Record<string, unknown>,
  prefix: string[] = [],
): EditableField[] {
  const out: EditableField[] = [];
  for (const key of Object.keys(config)) {
    const value = config[key];
    const path = [...prefix, key];
    if (typeof value === "string") {
      out.push(fieldAt(path, "string"));
    } else if (isStringArray(value)) {
      out.push(fieldAt(path, "array"));
    } else if (value && typeof value === "object" && !Array.isArray(value)) {
      out.push(...walkEditableFields(value as Record<string, unknown>, path));
    }
    // else: not an editable leaf — skip.
  }
  return out;
}

function fieldAt(path: string[], kind: FieldKind): EditableField {
  const leaf = path[path.length - 1] ?? "";
  const parent = path.slice(0, -1);
  return {
    path: path.join("."),
    leaf,
    parentLabel: parent.join(" › "),
    kind,
  };
}

function getPath(obj: Record<string, unknown>, path: string): unknown {
  let cur: unknown = obj;
  for (const p of path.split(".")) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[p];
  }
  return cur;
}

function setPath(target: Record<string, unknown>, path: string, value: unknown): void {
  const parts = path.split(".");
  const last = parts.length - 1;
  let cur: Record<string, unknown> = target;
  for (let i = 0; i < last; i++) {
    const p = parts[i];
    if (p === undefined) continue;
    const next = cur[p];
    if (next === null || typeof next !== "object" || Array.isArray(next)) {
      cur[p] = {};
    }
    cur = cur[p] as Record<string, unknown>;
  }
  const leaf = parts[last];
  if (leaf !== undefined) cur[leaf] = value;
}

function readStringAt(config: Record<string, unknown>, path: string): string {
  const v = getPath(config, path);
  return typeof v === "string" ? v : "";
}

function readArrayAt(config: Record<string, unknown>, path: string): string[] {
  const v = getPath(config, path);
  return isStringArray(v) ? v.slice() : [];
}

type Draft = Record<string, FieldValue>;

// initDraft projects the config into the form's editable values. Empty arrays
// default to a single blank row so a first-time editor sees a field to type in.
function initDraft(fields: EditableField[], config: Record<string, unknown>): Draft {
  const draft: Draft = {};
  for (const f of fields) {
    if (f.kind === "string") {
      draft[f.path] = readStringAt(config, f.path);
    } else {
      const arr = readArrayAt(config, f.path);
      draft[f.path] = arr.length > 0 ? arr : [""];
    }
  }
  return draft;
}

function normArray(a: string[]): string[] {
  return a.map((s) => s.trim()).filter((s) => s.length > 0);
}

function arraysEqual(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

function fieldChanged(field: EditableField, draft: Draft, baseline: Draft): boolean {
  const d = draft[field.path];
  const b = baseline[field.path];
  if (field.kind === "string") return (d as string) !== (b as string);
  return !arraysEqual(normArray(d as string[]), normArray(b as string[]));
}

// buildPatch turns the changed leaves into a sparse nested-object PATCH body.
// Strings are sent as-is (whitespace can be meaningful); arrays are trimmed
// with empty rows dropped.
function buildPatch(
  fields: EditableField[],
  draft: Draft,
  baseline: Draft,
): Record<string, unknown> {
  const patch: Record<string, unknown> = {};
  for (const f of fields) {
    if (!fieldChanged(f, draft, baseline)) continue;
    const value =
      f.kind === "string" ? (draft[f.path] as string) : normArray(draft[f.path] as string[]);
    setPath(patch, f.path, value);
  }
  return patch;
}

// ---------------------------------------------------------------------------
// Shell chrome — brand header + Sign out, matching RestrictedShell.
// ---------------------------------------------------------------------------

// Branding is the bot-declared editor title/description, surfaced once the
// share list loads so the header + heading can read "Éditeur de veilles"
// instead of the generic "Config editor".
interface Branding {
  title?: string;
  description?: string;
}

export default function ConfigEditorShell() {
  const { user, signOut, activeTeamID, activeTeam } = useAuth();
  const [branding, setBranding] = useState<Branding>({});
  const title = branding.title || "Config editor";
  return (
    <div className="flex h-screen min-h-0 flex-col bg-surface-0 text-fg-default">
      <header className="flex items-center justify-between border-b border-border-subtle px-4 py-3 sm:px-6">
        <div className="flex items-center gap-2.5">
          <BrandMark className="h-7 w-7" />
          <BrandWordmark />
          <span aria-hidden className="text-fg-subtle">
            /
          </span>
          <span className="text-sm font-medium text-fg-default">{title}</span>
        </div>
        <div className="flex items-center gap-2 sm:gap-3">
          <ThemeToggle />
          <span className="hidden text-xs text-fg-muted sm:inline">{user?.email}</span>
          <Button variant="secondary" size="sm" onClick={() => void signOut()}>
            Sign out
          </Button>
        </div>
      </header>

      <div className="min-h-0 flex-1 overflow-auto">
        <main className="mx-auto w-full max-w-5xl px-4 py-6 sm:px-6">
          {activeTeamID ? (
            <Workspace
              teamID={activeTeamID}
              teamName={activeTeam?.team_name}
              onBranding={setBranding}
            />
          ) : (
            <InlineBanner tone="warning" layout="inline" title="No active team">
              Your account has no active team, so there are no config-shares to edit.
              Ask an administrator to add you to a team.
            </InlineBanner>
          )}
        </main>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Workspace — share list (master) + editor (detail).
// ---------------------------------------------------------------------------

function Workspace({
  teamID,
  teamName,
  onBranding,
}: {
  teamID: string;
  teamName?: string;
  onBranding?: (b: { title?: string; description?: string }) => void;
}) {
  const [shares, setShares] = useState<EditorShare[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    try {
      const list = await listEditorShares(teamID);
      setShares(list);
      // Auto-select the first share so the editor isn't an empty right pane.
      setSelectedId((cur) => cur ?? list[0]?.id ?? null);
      // Surface the bot-declared editor branding (first share wins — a team's
      // shares are typically all one bot) up to the shell header + heading.
      onBranding?.({ title: list[0]?.editor_title, description: list[0]?.editor_description });
    } catch (err) {
      setShares([]);
      setError(errorMessage(err));
    }
  }, [teamID, onBranding]);

  useEffect(() => {
    void load();
  }, [load]);

  if (shares === null && !error) {
    return (
      <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
        <Spinner /> Loading config-shares…
      </div>
    );
  }

  const selected = shares?.find((s) => s.id === selectedId) ?? null;

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-lg font-semibold text-fg-default">
          {shares?.[0]?.editor_title || "Config editor"}
        </h1>
        <p className="text-sm text-fg-muted">
          {shares?.[0]?.editor_description ? (
            <>
              {shares[0].editor_description}
              {teamName ? (
                <>
                  {" "}
                  <span className="text-fg-subtle">
                    · <span className="font-medium text-fg-default">{teamName}</span>
                  </span>
                </>
              ) : null}
            </>
          ) : (
            <>
              Edit the config-shares
              {teamName ? (
                <>
                  {" "}for <span className="font-medium text-fg-default">{teamName}</span>
                </>
              ) : null}
              . Only the fields listed for each share are editable.
            </>
          )}
        </p>
      </div>

      {error && (
        <InlineBanner tone="danger" layout="inline" title="Couldn't load config-shares">
          {error}
          <div className="mt-2">
            <Button variant="secondary" size="sm" onClick={() => void load()}>
              Try again
            </Button>
          </div>
        </InlineBanner>
      )}

      {shares && shares.length === 0 && !error ? (
        <Card>
          <EmptyState
            title="No config-shares"
            message="This team has no config-shares assigned to you yet. Ask an administrator to create one."
          />
        </Card>
      ) : shares && shares.length > 0 ? (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-[minmax(220px,300px)_1fr]">
          <ShareList
            shares={shares}
            selectedId={selectedId}
            onSelect={(id) => setSelectedId(id)}
          />
          <div className="min-w-0">
            {selected ? (
              <ShareEditor key={selected.id} teamID={teamID} share={selected} />
            ) : (
              <Card>
                <EmptyState message="Select a config-share on the left to edit it." />
              </Card>
            )}
          </div>
        </div>
      ) : null}
    </div>
  );
}

function ShareList({
  shares,
  selectedId,
  onSelect,
}: {
  shares: EditorShare[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <ul className="flex flex-col gap-2" aria-label="Config-shares">
      {shares.map((s) => {
        const active = s.id === selectedId;
        return (
          <li key={s.id}>
            <button
              type="button"
              onClick={() => onSelect(s.id)}
              aria-current={active ? "true" : undefined}
              className={`w-full rounded-[var(--radius-lg)] border px-3 py-2 text-left transition-colors ${
                active
                  ? "border-accent bg-accent-soft/50"
                  : "border-border-default bg-surface-1 hover:border-border-strong"
              }`}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="truncate text-sm font-medium text-fg-default">
                  {s.label || s.bot_id || s.id}
                </span>
                {s.read_only && (
                  <span className="shrink-0 text-caption text-fg-subtle">read-only</span>
                )}
              </div>
              <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-caption text-fg-subtle">
                {s.bot_id && <span className="truncate">{s.bot_id}</span>}
                {s.category && (
                  <>
                    <span aria-hidden>·</span>
                    <span className="truncate">{s.category}</span>
                  </>
                )}
              </div>
              {s.config_path && (
                <div className="mt-0.5 truncate font-mono text-caption text-fg-subtle">
                  {s.config_path}
                </div>
              )}
            </button>
          </li>
        );
      })}
    </ul>
  );
}

// ---------------------------------------------------------------------------
// ShareEditor — load one share's projected config, edit its fields, save.
// Remounted (key={share.id}) on selection so all state resets cleanly.
// ---------------------------------------------------------------------------

type SaveStatus =
  | { kind: "idle" }
  | { kind: "saved"; changed: number }
  | { kind: "error"; message: string };

function ShareEditor({ teamID, share }: { teamID: string; share: EditorShare }) {
  const [meta, setMeta] = useState<EditorConfigResponse | null>(null);
  const [fields, setFields] = useState<EditableField[]>([]);
  const [baseline, setBaseline] = useState<Draft>({});
  const [draft, setDraft] = useState<Draft>({});
  const [sha, setSha] = useState("");
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState<SaveStatus>({ kind: "idle" });
  const [conflict, setConflict] = useState<{ serverDraft: Draft; serverSha: string } | null>(
    null,
  );
  const bootRef = useRef(false);

  const bootstrap = useCallback(async () => {
    setLoadError(null);
    try {
      const cfg = await getEditorConfig(teamID, share.id);
      const fs = walkEditableFields(cfg.config);
      const d = initDraft(fs, cfg.config);
      setMeta(cfg);
      setFields(fs);
      setBaseline(d);
      setDraft(d);
      setSha(cfg.sha);
    } catch (err) {
      setLoadError(errorMessage(err));
    }
  }, [teamID, share.id]);

  useEffect(() => {
    if (bootRef.current) return;
    bootRef.current = true;
    void bootstrap();
  }, [bootstrap]);

  const readOnly = meta?.read_only ?? share.read_only;
  const dirty = useMemo(
    () => fields.some((f) => fieldChanged(f, draft, baseline)),
    [fields, draft, baseline],
  );

  const setField = useCallback((path: string, value: FieldValue) => {
    setDraft((prev) => ({ ...prev, [path]: value }));
    setStatus({ kind: "idle" });
  }, []);

  const onSave = useCallback(
    async (forcedSha?: string) => {
      if (readOnly) return;
      const patch = buildPatch(fields, draft, baseline);
      if (Object.keys(patch).length === 0) return;
      setSaving(true);
      setStatus({ kind: "idle" });
      try {
        const result = await patchEditorConfig(teamID, share.id, {
          sha: forcedSha ?? sha,
          patch,
        });
        if (result.kind === "conflict") {
          setConflict({
            serverDraft: initDraft(fields, result.config),
            serverSha: result.sha,
          });
          setStatus({
            kind: "error",
            message: "Someone else edited this file — review the differences before saving.",
          });
          return;
        }
        if (result.kind === "not_editable") {
          setStatus({ kind: "error", message: result.message });
          return;
        }
        setBaseline({ ...draft });
        setSha(result.sha);
        setStatus({ kind: "saved", changed: result.changed.length });
        setConflict(null);
      } catch (err) {
        setStatus({ kind: "error", message: errorMessage(err) });
      } finally {
        setSaving(false);
      }
    },
    [readOnly, fields, draft, baseline, teamID, share.id, sha],
  );

  if (loadError) {
    return (
      <InlineBanner tone="danger" layout="inline" title="Couldn't load this config-share">
        {loadError}
        <div className="mt-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              bootRef.current = false;
              void bootstrap();
            }}
          >
            Try again
          </Button>
        </div>
      </InlineBanner>
    );
  }

  if (!meta) {
    return (
      <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
        <Spinner /> Loading configuration…
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <InlineBanner
        tone="info"
        layout="inline"
        title={`Scoped editor — ${share.label || share.bot_id || share.id}`}
      >
        Editing {share.category ? <strong>{share.category}</strong> : "the config"} of{" "}
        <code className="font-mono">{meta.config_path || share.config_path}</code>. Only the
        fields below are editable — everything else in the file is out of scope.
      </InlineBanner>

      {readOnly && (
        <InlineBanner tone="info" layout="inline" title="Read-only">
          You have read-only access to this share. You can review the values below but Save
          is disabled.
        </InlineBanner>
      )}

      <CadenceCard teamID={teamID} share={share} readOnly={readOnly} />

      {fields.length === 0 ? (
        <Card>
          <EmptyState
            title="Nothing to edit yet"
            message={`This ${
              share.category ? `"${share.category}" ` : ""
            }section isn't set up in the config file yet, so there are no fields to edit. Ask an administrator to add it.`}
          />
        </Card>
      ) : (
        fields.map((f) =>
          f.kind === "string" ? (
            <StringField
              key={f.path}
              field={f}
              value={draft[f.path] as string}
              disabled={readOnly}
              onChange={(v) => setField(f.path, v)}
            />
          ) : (
            <ArrayField
              key={f.path}
              field={f}
              value={draft[f.path] as string[]}
              disabled={readOnly}
              onChange={(v) => setField(f.path, v)}
            />
          ),
        )
      )}

      {fields.length > 0 && (
        <div className="sticky bottom-0 -mx-4 border-t border-border-subtle bg-surface-0/95 px-4 py-3 backdrop-blur sm:-mx-6 sm:px-6">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <StatusLine status={status} readOnly={readOnly} dirty={dirty} />
            <div className="flex items-center gap-2">
              <Button
                variant="ghost"
                size="sm"
                disabled={saving || !dirty || readOnly}
                onClick={() => {
                  setDraft(baseline);
                  setStatus({ kind: "idle" });
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
      )}

      {conflict && (
        <ConflictDialog
          fields={fields}
          yours={draft}
          server={conflict.serverDraft}
          onCancel={() => setConflict(null)}
          onOverwrite={() => void onSave(conflict.serverSha)}
          onAdoptServer={() => {
            setDraft(conflict.serverDraft);
            setBaseline(conflict.serverDraft);
            setSha(conflict.serverSha);
            setConflict(null);
            setStatus({ kind: "idle" });
          }}
        />
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// CadenceCard — edit the cron of the schedule bound to this share's category.
// The recurrence lives in iterion's schedule store (visible in the Schedules
// view), NOT the repo config. Self-hides when the category has no schedule or
// the server has no scheduler (local mode) — it never breaks the content editor.
// ---------------------------------------------------------------------------

const CRON_PRESETS: { label: string; expr: string }[] = [
  { label: "Daily 08:00", expr: "0 8 * * *" },
  { label: "Weekdays 08:00", expr: "0 8 * * 1-5" },
  { label: "Weekly · Mon 08:00", expr: "0 8 * * 1" },
  { label: "Weekly · Wed 08:00", expr: "0 8 * * 3" },
];

// splitCronTZ preserves an optional "CRON_TZ=…" prefix so a preset only rewrites
// the schedule fields, keeping the timezone the operator set on the schedule.
function splitCronTZ(cron: string): { tz: string; expr: string } {
  const m = cron.match(/^(CRON_TZ=\S+\s+)([\s\S]*)$/);
  return m ? { tz: m[1] ?? "", expr: (m[2] ?? "").trim() } : { tz: "", expr: cron.trim() };
}

function formatNextFire(iso?: string): string | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d.toLocaleString();
}

function CadenceCard({
  teamID,
  share,
  readOnly,
}: {
  teamID: string;
  share: EditorShare;
  readOnly: boolean;
}) {
  const [loaded, setLoaded] = useState(false);
  const [hidden, setHidden] = useState(false);
  const [cron, setCron] = useState("");
  const [baseline, setBaseline] = useState("");
  const [nextFire, setNextFire] = useState<string | undefined>(undefined);
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState<SaveStatus>({ kind: "idle" });
  const bootRef = useRef(false);

  useEffect(() => {
    if (bootRef.current) return;
    bootRef.current = true;
    void (async () => {
      try {
        const sched = await getEditorSchedule(teamID, share.id);
        if (!sched.exists || !sched.cron) {
          setHidden(true);
          return;
        }
        setCron(sched.cron);
        setBaseline(sched.cron);
        setNextFire(sched.next_fire_at);
        setLoaded(true);
      } catch {
        // No scheduler on this server, or the schedule read failed: the cadence
        // simply isn't editable here — hide the card, never break the editor.
        setHidden(true);
      }
    })();
  }, [teamID, share.id]);

  const dirty = cron.trim() !== baseline.trim();

  const setPreset = (expr: string) => {
    setCron(splitCronTZ(cron).tz + expr);
    setStatus({ kind: "idle" });
  };

  const onSave = async () => {
    const c = cron.trim();
    if (!c || c === baseline.trim()) return;
    setSaving(true);
    setStatus({ kind: "idle" });
    try {
      const r = await patchEditorSchedule(teamID, share.id, c);
      setCron(r.cron);
      setBaseline(r.cron);
      setNextFire(r.next_fire_at);
      setStatus({ kind: "saved", changed: 1 });
    } catch (err) {
      setStatus({ kind: "error", message: errorMessage(err) });
    } finally {
      setSaving(false);
    }
  };

  if (hidden || !loaded) return null;

  const next = formatNextFire(nextFire);
  return (
    <Card>
      <FieldLabel help="How often the digest is published — kept in the Schedules view">
        Cadence
      </FieldLabel>
      {!readOnly && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {CRON_PRESETS.map((p) => (
            <Button key={p.expr} variant="secondary" size="sm" onClick={() => setPreset(p.expr)}>
              {p.label}
            </Button>
          ))}
        </div>
      )}
      <Input
        value={cron}
        disabled={readOnly}
        onChange={(e) => {
          setCron(e.target.value);
          setStatus({ kind: "idle" });
        }}
        spellCheck={false}
        autoComplete="off"
        aria-label="Cron expression"
        className="font-mono"
      />
      <div className="mt-1.5 flex flex-wrap items-center justify-between gap-2">
        <span className="text-caption text-fg-subtle">
          {next ? `Next run: ${next}` : "cron expression, e.g. 0 8 * * 1"}
        </span>
        {!readOnly && (
          <div className="flex items-center gap-2">
            {status.kind === "saved" && (
              <span className="text-xs text-success-fg">Cadence saved</span>
            )}
            {status.kind === "error" && (
              <span className="text-xs text-danger-fg">{status.message}</span>
            )}
            <Button
              variant="secondary"
              size="sm"
              loading={saving}
              disabled={saving || !dirty}
              onClick={() => void onSave()}
            >
              Save cadence
            </Button>
          </div>
        )}
      </div>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Field widgets — a plain textarea for strings, an add/remove list for arrays.
// ---------------------------------------------------------------------------

function StringField({
  field,
  value,
  disabled,
  onChange,
}: {
  field: EditableField;
  value: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  const multiline = value.includes("\n") || value.length > 80;
  return (
    <Card>
      <FieldLabel help={field.parentLabel || undefined}>{field.leaf}</FieldLabel>
      <Textarea
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        rows={multiline ? 8 : 3}
        className="font-mono"
      />
    </Card>
  );
}

function ArrayField({
  field,
  value,
  disabled,
  onChange,
}: {
  field: EditableField;
  value: string[];
  disabled: boolean;
  onChange: (v: string[]) => void;
}) {
  // Feed lists are the common case and get url-typed inputs + validation;
  // any other array leaf falls back to plain text rows.
  const isFeeds = field.leaf === "feeds";
  const rows = value.length > 0 ? value : [""];
  const setAt = (i: number, v: string) => {
    const next = rows.slice();
    next[i] = v;
    onChange(next);
  };
  const removeAt = (i: number) => {
    const next = rows.slice();
    next.splice(i, 1);
    if (next.length === 0) next.push("");
    onChange(next);
  };
  const count = rows.filter((r) => r.trim().length > 0).length;
  return (
    <Card>
      <div className="mb-2 flex items-baseline justify-between">
        <FieldLabel help={field.parentLabel || undefined}>{field.leaf}</FieldLabel>
        <span className="text-caption text-fg-subtle">
          {count} item{count === 1 ? "" : "s"}
        </span>
      </div>
      <ul className="flex flex-col gap-2">
        {rows.map((item, i) => {
          const trimmed = item.trim();
          const err = isFeeds && trimmed.length > 0 && !httpUrlPattern.test(trimmed);
          return (
            <li key={i} className="flex items-center gap-2">
              <Input
                type={isFeeds ? "url" : "text"}
                inputMode={isFeeds ? "url" : undefined}
                autoComplete="off"
                spellCheck={false}
                size="md"
                value={item}
                error={err}
                disabled={disabled}
                placeholder={isFeeds ? "https://example.org/feed.xml" : ""}
                aria-label={`${field.leaf} ${i + 1}`}
                onChange={(e) => setAt(i, e.target.value)}
                className="font-mono"
              />
              <Button
                variant="ghost"
                size="sm"
                disabled={disabled}
                onClick={() => removeAt(i)}
                aria-label={`Remove ${field.leaf} ${i + 1}`}
              >
                Remove
              </Button>
            </li>
          );
        })}
      </ul>
      <div className="mt-2">
        <Button
          variant="secondary"
          size="sm"
          disabled={disabled}
          onClick={() => onChange([...rows, ""])}
        >
          + Add
        </Button>
      </div>
    </Card>
  );
}

function StatusLine({
  status,
  readOnly,
  dirty,
}: {
  status: SaveStatus;
  readOnly: boolean;
  dirty: boolean;
}) {
  if (readOnly) return <span className="text-xs text-fg-subtle">Read-only share</span>;
  if (status.kind === "saved") {
    return (
      <span className="text-xs text-success-fg">
        Saved · {status.changed} field{status.changed === 1 ? "" : "s"} updated
      </span>
    );
  }
  if (status.kind === "error") {
    return <span className="text-xs text-danger-fg">{status.message}</span>;
  }
  return (
    <span className="text-xs text-fg-subtle">{dirty ? "Unsaved changes" : "No changes"}</span>
  );
}

// ---------------------------------------------------------------------------
// Conflict resolution — explicit user action, never a silent retry.
// ---------------------------------------------------------------------------

function ConflictDialog({
  fields,
  yours,
  server,
  onCancel,
  onOverwrite,
  onAdoptServer,
}: {
  fields: EditableField[];
  yours: Draft;
  server: Draft;
  onCancel: () => void;
  onOverwrite: () => void;
  onAdoptServer: () => void;
}) {
  // Only surface the leaves that actually differ between the two versions.
  const diffed = useMemo(
    () => fields.filter((f) => fieldChanged(f, yours, server)),
    [fields, yours, server],
  );
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
      <div className="flex flex-col gap-3">
        {diffed.map((f) => (
          <div key={f.path}>
            <div className="mb-1 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
              {f.parentLabel ? `${f.parentLabel} › ${f.leaf}` : f.leaf}
            </div>
            <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
              <ConflictPane title="Your draft" field={f} value={yours[f.path]} />
              <ConflictPane
                title="Server version (current)"
                field={f}
                value={server[f.path]}
                highlight
              />
            </div>
          </div>
        ))}
      </div>
    </Dialog>
  );
}

function ConflictPane({
  title,
  field,
  value,
  highlight = false,
}: {
  title: string;
  field: EditableField;
  value?: FieldValue;
  highlight?: boolean;
}) {
  return (
    <div
      className={`rounded-md border p-3 text-xs ${
        highlight ? "border-accent bg-accent-soft/50" : "border-border-default bg-surface-2"
      }`}
    >
      <h3 className="mb-1 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
        {title}
      </h3>
      {field.kind === "array" ? (
        (() => {
          const items = normArray(Array.isArray(value) ? value : []);
          return items.length === 0 ? (
            <p className="italic text-fg-subtle">empty</p>
          ) : (
            <ul className="space-y-0.5 font-mono">
              {items.map((it, i) => (
                <li key={i} className="truncate">
                  {it}
                </li>
              ))}
            </ul>
          );
        })()
      ) : (
        <pre className="whitespace-pre-wrap wrap-break-word font-mono text-fg-default">
          {(typeof value === "string" ? value : "") || (
            <span className="italic text-fg-subtle">empty</span>
          )}
        </pre>
      )}
    </div>
  );
}
