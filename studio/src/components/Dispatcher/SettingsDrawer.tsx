import { useEffect, useState } from "react";

import * as dispatcher from "@/api/dispatcher";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Drawer } from "@/components/ui/Drawer";
import { IconButton } from "@/components/ui/IconButton";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Spinner } from "@/components/ui/Spinner";
import { useAsyncAction } from "@/hooks/useAsyncAction";

interface Props {
  open: boolean;
  onClose: () => void;
  onSaved: () => void;
}

// SettingsDrawer is the form-driven editor for dispatcher.json. It
// loads the current config from /api/v1/dispatcher/config on open and
// PUTs the result on save. No YAML editing required.
export default function SettingsDrawer({ open, onClose, onSaved }: Props) {
  const [cfg, setCfg] = useState<dispatcher.DispatcherConfig | null>(null);
  // Two useAsyncAction slots: one for the open-time load, one for the
  // user-triggered Save. They share the same InlineBanner so the user
  // sees the most recent failure, but their `busy` flags are
  // independent — the header pill reflects load, the Save button
  // reflects save.
  const loadAction = useAsyncAction();
  const saveAction = useAsyncAction();
  const loading = loadAction.busy;
  const saving = saveAction.busy;
  const error = saveAction.error ?? loadAction.error;

  useEffect(() => {
    if (!open) return;
    let alive = true;
    void loadAction.run(async () => {
      const c = await dispatcher.getConfig();
      if (alive) setCfg(c ?? defaultConfig());
    });
    return () => {
      alive = false;
    };
    // loadAction is recreated on each render but its run/setError are
    // stable; depending only on `open` matches the prior behaviour
    // (the IIFE-style load fired exactly on open transitions).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const onSave = () =>
    saveAction.run(async () => {
      if (!cfg) return;
      await dispatcher.saveConfig(cfg);
      onSaved();
      onClose();
    });

  return (
    <Drawer
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title="Dispatcher settings"
      widthClass="max-w-3xl"
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            disabled={!cfg}
            loading={saving}
            onClick={() => void onSave()}
          >
            Save
          </Button>
        </>
      }
    >
      <div className="space-y-3 text-sm">
        {loading && (
          <span
            className="inline-flex items-center gap-1.5 text-xs text-fg-muted"
            aria-live="polite"
          >
            <Spinner size="sm" />
            <span>loading…</span>
          </span>
        )}

        {error && <InlineBanner tone="danger">{error}</InlineBanner>}

        {cfg && <Form cfg={cfg} setCfg={setCfg} />}
      </div>
    </Drawer>
  );
}

function defaultConfig(): dispatcher.DispatcherConfig {
  return {
    name: "",
    workflow: "",
    tracker: { kind: "native" },
    dispatch: { vars: {} },
    polling: { interval_ms: 30000 },
    agent: { max_concurrent: 2, max_retry_backoff_ms: 300000 },
    workspace: { root: "", persist: "cleanup_on_done" },
    hooks: {},
    stall: { timeout_ms: 600000 },
  };
}

// ---------------------------------------------------------------------------
// Form sections
// ---------------------------------------------------------------------------

interface FormProps {
  cfg: dispatcher.DispatcherConfig;
  setCfg: (c: dispatcher.DispatcherConfig) => void;
}

function Form({ cfg, setCfg }: FormProps) {
  const update = <K extends keyof dispatcher.DispatcherConfig>(
    k: K,
    v: dispatcher.DispatcherConfig[K],
  ) => setCfg({ ...cfg, [k]: v });

  return (
    <div className="space-y-6">
      <Section title="General">
        <Field label="Name" hint="Display label in logs and dashboards">
          <Input
            size="sm"
            value={cfg.name ?? ""}
            onChange={(e) => update("name", e.target.value)}
            placeholder="my-dispatcher"
          />
        </Field>
        <Field
          label="Workflow path"
          hint=".bot, .botz, or unpacked-bundle dir. Path is resolved relative to the project root."
        >
          <Input
            size="sm"
            value={cfg.workflow}
            onChange={(e) => update("workflow", e.target.value)}
            placeholder="./bots/feature-dev/main.bot"
          />
        </Field>
      </Section>

      <Section title="Tracker">
        <Field label="Kind">
          <Select
            size="sm"
            value={cfg.tracker.kind}
            onChange={(e) => update("tracker", { ...cfg.tracker, kind: e.target.value as dispatcher.TrackerKind })}
          >
            <option value="native">native (local kanban)</option>
            <option value="github">github</option>
            <option value="forgejo">forgejo / gitea</option>
          </Select>
        </Field>
        {cfg.tracker.kind === "github" && (
          <GitHubTrackerFields
            cfg={cfg.tracker.github ?? { repo: "" }}
            set={(g) => update("tracker", { ...cfg.tracker, github: g })}
          />
        )}
        {cfg.tracker.kind === "forgejo" && (
          <ForgejoTrackerFields
            cfg={cfg.tracker.forgejo ?? { host: "", repo: "" }}
            set={(f) => update("tracker", { ...cfg.tracker, forgejo: f })}
          />
        )}
      </Section>

      <Section title="Dispatch — workflow vars">
        <p className="text-xs text-fg-muted">
          Map each <code>vars:</code> declared in the workflow to a template. Use{" "}
          <code>{"{{issue.title}}"}</code>, <code>{"{{issue.body}}"}</code>,{" "}
          <code>{"{{dispatcher.workspace_path}}"}</code>, etc.
        </p>
        <KVTable
          rows={cfg.dispatch?.vars ?? {}}
          onChange={(vars) => update("dispatch", { ...(cfg.dispatch ?? {}), vars })}
          keyPlaceholder="feature_prompt"
          valuePlaceholder="Issue {{issue.identifier}}: {{issue.title}}"
        />
      </Section>

      <Section title="Polling">
        <Field
          label="Interval (ms)"
          hint="How often the dispatcher checks the tracker. Default 30 000 ms (30 s)."
        >
          <Input
            size="sm"
            type="number"
            min={1000}
            value={cfg.polling?.interval_ms ?? 30000}
            onChange={(e) => update("polling", { interval_ms: Number(e.target.value) })}
          />
        </Field>
      </Section>

      <Section title="Concurrency & retry">
        <Field label="Max concurrent dispatches">
          <Input
            size="sm"
            type="number"
            min={1}
            value={cfg.agent?.max_concurrent ?? 2}
            onChange={(e) =>
              update("agent", { ...(cfg.agent ?? {}), max_concurrent: Number(e.target.value) })
            }
          />
        </Field>
        <Field label="Max retry backoff (ms)" hint="Exponential cap. Default 300000 (5min)">
          <Input
            size="sm"
            type="number"
            min={1000}
            value={cfg.agent?.max_retry_backoff_ms ?? 300000}
            onChange={(e) =>
              update("agent", { ...(cfg.agent ?? {}), max_retry_backoff_ms: Number(e.target.value) })
            }
          />
        </Field>
      </Section>

      <Section title="Workspace">
        <Field
          label="Root"
          hint="Per-issue workspaces live here. Empty = <store-dir>/dispatcher/workspaces."
        >
          <Input
            size="sm"
            value={cfg.workspace?.root ?? ""}
            onChange={(e) => update("workspace", { ...(cfg.workspace ?? {}), root: e.target.value })}
            placeholder="~/.iterion/dispatcher-workspaces"
          />
        </Field>
        <Field label="Persistence">
          <Select
            size="sm"
            value={cfg.workspace?.persist ?? ""}
            onChange={(e) =>
              update("workspace", {
                ...(cfg.workspace ?? {}),
                persist: e.target.value as dispatcher.WorkspacePersistPolicy,
              })
            }
          >
            <option value="">keep (manual cleanup)</option>
            <option value="cleanup_on_done">cleanup on successful run</option>
            <option value="cleanup_on_terminal">cleanup on terminal state</option>
          </Select>
        </Field>
      </Section>

      <Section title="Hooks">
        <p className="text-xs text-fg-muted">
          Inline shell snippets that run with cwd = workspace. Leave blank to skip.
          Use them to clone the issue&apos;s source, seed credentials, or run a
          teardown.
        </p>
        <HookFields
          label="after_create"
          value={cfg.hooks?.after_create ?? null}
          onChange={(h) => update("hooks", { ...(cfg.hooks ?? {}), after_create: h })}
        />
        <HookFields
          label="before_run"
          value={cfg.hooks?.before_run ?? null}
          onChange={(h) => update("hooks", { ...(cfg.hooks ?? {}), before_run: h })}
        />
        <HookFields
          label="after_run"
          value={cfg.hooks?.after_run ?? null}
          onChange={(h) => update("hooks", { ...(cfg.hooks ?? {}), after_run: h })}
        />
        <HookFields
          label="before_remove"
          value={cfg.hooks?.before_remove ?? null}
          onChange={(h) => update("hooks", { ...(cfg.hooks ?? {}), before_remove: h })}
        />
      </Section>

      <Section title="Stall detection">
        <Field
          label="Timeout (ms)"
          hint="If a run is silent for this long, the dispatcher cancels and retries it. 0 disables stall detection."
        >
          <Input
            size="sm"
            type="number"
            min={0}
            value={cfg.stall?.timeout_ms ?? 600000}
            onChange={(e) => update("stall", { timeout_ms: Number(e.target.value) })}
          />
        </Field>
      </Section>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Sub-components
// ---------------------------------------------------------------------------

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="space-y-2 rounded border border-border-default bg-surface-1 p-3">
      <h3 className="text-xs font-semibold uppercase tracking-wide text-fg-muted">{title}</h3>
      <div className="space-y-3">{children}</div>
    </section>
  );
}

function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <label className="block space-y-1">
      <span className="text-xs font-medium text-fg-default">{label}</span>
      {children}
      {hint && <span className="block text-xs text-fg-muted">{hint}</span>}
    </label>
  );
}

function GitHubTrackerFields({
  cfg,
  set,
}: {
  cfg: dispatcher.GitHubTrackerConfig;
  set: (c: dispatcher.GitHubTrackerConfig) => void;
}) {
  return (
    <>
      <Field label="Repo" hint="owner/repo">
        <Input size="sm" value={cfg.repo} onChange={(e) => set({ ...cfg, repo: e.target.value })} placeholder="SocialGouv/iterion" />
      </Field>
      <Field label="Token" hint="Env var name (e.g. $GITHUB_TOKEN) or literal. Leave blank to use `gh auth token`.">
        <Input size="sm" value={cfg.token ?? ""} onChange={(e) => set({ ...cfg, token: e.target.value })} placeholder="$GITHUB_TOKEN" />
      </Field>
      <Field label="Claimed label" hint="Label added to claimed issues. Default 'iterion-claimed'.">
        <Input size="sm" value={cfg.claimed_label ?? ""} onChange={(e) => set({ ...cfg, claimed_label: e.target.value })} placeholder="iterion-claimed" />
      </Field>
      <LabelListField label="Include labels (AND filter)" value={cfg.include_labels ?? []} onChange={(v) => set({ ...cfg, include_labels: v })} />
      <LabelListField label="Exclude labels" value={cfg.exclude_labels ?? []} onChange={(v) => set({ ...cfg, exclude_labels: v })} />
      <StateMappingField value={cfg.state_mapping ?? {}} onChange={(v) => set({ ...cfg, state_mapping: v })} />
    </>
  );
}

function ForgejoTrackerFields({
  cfg,
  set,
}: {
  cfg: dispatcher.ForgejoTrackerConfig;
  set: (c: dispatcher.ForgejoTrackerConfig) => void;
}) {
  return (
    <>
      <Field label="Host" hint="Base URL, e.g. https://codeberg.org">
        <Input size="sm" value={cfg.host} onChange={(e) => set({ ...cfg, host: e.target.value })} placeholder="https://codeberg.org" />
      </Field>
      <Field label="Repo" hint="owner/repo">
        <Input size="sm" value={cfg.repo} onChange={(e) => set({ ...cfg, repo: e.target.value })} placeholder="forgejo/forgejo" />
      </Field>
      <Field label="Token">
        <Input size="sm" value={cfg.token ?? ""} onChange={(e) => set({ ...cfg, token: e.target.value })} placeholder="$FORGEJO_TOKEN" />
      </Field>
      <Field label="Claimed label">
        <Input size="sm" value={cfg.claimed_label ?? ""} onChange={(e) => set({ ...cfg, claimed_label: e.target.value })} placeholder="iterion-claimed" />
      </Field>
      <LabelListField label="Include labels" value={cfg.include_labels ?? []} onChange={(v) => set({ ...cfg, include_labels: v })} />
      <LabelListField label="Exclude labels" value={cfg.exclude_labels ?? []} onChange={(v) => set({ ...cfg, exclude_labels: v })} />
      <StateMappingField value={cfg.state_mapping ?? {}} onChange={(v) => set({ ...cfg, state_mapping: v })} />
    </>
  );
}

function LabelListField({
  label,
  value,
  onChange,
}: {
  label: string;
  value: string[];
  onChange: (v: string[]) => void;
}) {
  return (
    <Field label={label} hint="Comma-separated">
      <Input
        size="sm"
        value={value.join(", ")}
        onChange={(e) =>
          onChange(
            e.target.value
              .split(",")
              .map((s) => s.trim())
              .filter(Boolean),
          )
        }
      />
    </Field>
  );
}

function StateMappingField({
  value,
  onChange,
}: {
  value: Record<string, dispatcher.LabelSelector>;
  onChange: (v: Record<string, dispatcher.LabelSelector>) => void;
}) {
  const entries = Object.entries(value);
  const setKey = (oldKey: string, newKey: string) => {
    const next: Record<string, dispatcher.LabelSelector> = {};
    for (const [k, v] of entries) {
      next[k === oldKey ? newKey : k] = v;
    }
    onChange(next);
  };
  const setSelector = (key: string, sel: dispatcher.LabelSelector) => {
    onChange({ ...value, [key]: sel });
  };
  const remove = (key: string) => {
    const next = { ...value };
    delete next[key];
    onChange(next);
  };
  return (
    <div className="space-y-2">
      <span className="text-xs font-medium">State mapping (workflow state → label selector)</span>
      {entries.map(([state, sel]) => (
        <div key={state} className="space-y-1 rounded border border-border-default bg-surface-2 p-2">
          <Input
            size="sm"
            value={state}
            onChange={(e) => setKey(state, e.target.value)}
            placeholder="ready"
          />
          <Input
            size="sm"
            placeholder="includes (comma-separated)"
            value={(sel.labels_include ?? []).join(", ")}
            onChange={(e) =>
              setSelector(state, {
                ...sel,
                labels_include: e.target.value.split(",").map((s) => s.trim()).filter(Boolean),
              })
            }
          />
          <Input
            size="sm"
            placeholder="excludes (comma-separated)"
            value={(sel.labels_exclude ?? []).join(", ")}
            onChange={(e) =>
              setSelector(state, {
                ...sel,
                labels_exclude: e.target.value.split(",").map((s) => s.trim()).filter(Boolean),
              })
            }
          />
          <Button
            variant="ghost"
            size="sm"
            className="text-danger hover:text-danger"
            onClick={() => remove(state)}
          >
            Remove
          </Button>
        </div>
      ))}
      <Button
        variant="ghost"
        size="sm"
        onClick={() =>
          onChange({ ...value, "": { labels_include: [], labels_exclude: [] } })
        }
      >
        + Add state mapping
      </Button>
    </div>
  );
}

function KVTable({
  rows,
  onChange,
  keyPlaceholder,
  valuePlaceholder,
}: {
  rows: Record<string, string>;
  onChange: (v: Record<string, string>) => void;
  keyPlaceholder?: string;
  valuePlaceholder?: string;
}) {
  const entries = Object.entries(rows);
  const setKey = (oldKey: string, newKey: string) => {
    const next: Record<string, string> = {};
    for (const [k, v] of entries) next[k === oldKey ? newKey : k] = v;
    onChange(next);
  };
  const setVal = (k: string, val: string) => onChange({ ...rows, [k]: val });
  const remove = (k: string) => {
    const next = { ...rows };
    delete next[k];
    onChange(next);
  };
  return (
    <div className="space-y-2">
      {entries.map(([k, v]) => (
        <div key={k} className="flex gap-2">
          <Input
            size="sm"
            className="w-32"
            value={k}
            placeholder={keyPlaceholder}
            onChange={(e) => setKey(k, e.target.value)}
          />
          <textarea
            className="input flex-1 min-h-[2.25rem]"
            rows={1}
            value={v}
            placeholder={valuePlaceholder}
            onChange={(e) => setVal(k, e.target.value)}
          />
          <IconButton
            size="sm"
            variant="danger"
            label={k ? `Remove ${k}` : "Remove row"}
            onClick={() => remove(k)}
          >
            ×
          </IconButton>
        </div>
      ))}
      <Button
        variant="ghost"
        size="sm"
        onClick={() => onChange({ ...rows, "": "" })}
      >
        + Add var
      </Button>
    </div>
  );
}

function HookFields({
  label,
  value,
  onChange,
}: {
  label: string;
  value: dispatcher.HookSpec | null;
  onChange: (h: dispatcher.HookSpec | null) => void;
}) {
  const enabled = value != null;
  const toggle = (on: boolean) => {
    if (on) onChange({ script: "", timeout_ms: 60000 });
    else onChange(null);
  };
  return (
    <div className="space-y-1 rounded border border-border-default bg-surface-2 p-2">
      <label className="flex items-center gap-2 text-xs">
        <Checkbox checked={enabled} onChange={(e) => toggle(e.target.checked)} />
        <span className="font-mono">{label}</span>
      </label>
      {enabled && value && (
        <>
          <textarea
            className="input min-h-[3rem] font-mono text-xs"
            rows={3}
            value={value.script ?? ""}
            placeholder="git clone $REPO ."
            onChange={(e) => onChange({ ...value, script: e.target.value, path: undefined })}
          />
          <div className="flex gap-2">
            <Input
              size="sm"
              className="flex-1"
              placeholder="… or path to a script"
              value={value.path ?? ""}
              onChange={(e) => onChange({ ...value, path: e.target.value, script: undefined })}
            />
            <Input
              size="sm"
              className="w-32"
              type="number"
              min={1000}
              value={value.timeout_ms ?? 60000}
              onChange={(e) => onChange({ ...value, timeout_ms: Number(e.target.value) })}
            />
          </div>
        </>
      )}
    </div>
  );
}
