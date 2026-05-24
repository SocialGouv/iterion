// Labels view — operator surface for vocabulary management.
//
// The native board accumulates labels from many sources (whats-next
// emit_action, sec-audit findings, manual triage). Without a
// dedicated surface the only options for an operator who notices two
// labels mean the same thing have been: edit every issue by hand, or
// hand-craft a curl loop. This view exposes the three label-ops the
// store grew alongside list_labels — rename / merge / delete — with a
// preview of how many issues each op would touch and a confirm step
// (delete + merge are destructive enough that a stray double-click
// should not propagate across N issues silently).
//
// The label vocabulary discipline is documented in the
// `iterion-label-vocabulary` skill; this view is the human-facing
// counterpart that bots read via `mcp__iterion_board__list_labels`.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation } from "wouter";

import {
  deleteLabel,
  listLabels,
  mergeLabels,
  renameLabel,
  type LabelUsage,
} from "@/api/native";
import { Button } from "@/components/ui/Button";
import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import { formatRelative } from "@/lib/format";

type DialogState =
  | { kind: "none" }
  | { kind: "rename"; label: LabelUsage; nextValue: string }
  | { kind: "merge"; label: LabelUsage; nextValue: string }
  | { kind: "delete"; label: LabelUsage };

export default function LabelsView() {
  return (
    <ErrorBoundary area="Labels view">
      <LabelsViewInner />
    </ErrorBoundary>
  );
}

function LabelsViewInner() {
  const [, setLocation] = useLocation();
  const [labels, setLabels] = useState<LabelUsage[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [searchQuery, setSearchQuery] = useState("");
  const [dialog, setDialog] = useState<DialogState>({ kind: "none" });
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const next = await listLabels();
      setLabels(next);
      setError(null);
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const filtered = useMemo(() => {
    if (!labels) return null;
    const q = searchQuery.trim().toLowerCase();
    if (!q) return labels;
    return labels.filter((l) => l.label.toLowerCase().includes(q));
  }, [labels, searchQuery]);

  // Namespace grouping ("source:", "horizon:", …) makes the catalogue
  // legible at a glance — operators scan one namespace at a time when
  // hunting duplicates. Unprefixed labels go to a "(no namespace)"
  // bucket so they don't disappear.
  const grouped = useMemo(() => {
    if (!filtered) return null;
    const buckets = new Map<string, LabelUsage[]>();
    for (const l of filtered) {
      const idx = l.label.indexOf(":");
      const ns = idx > 0 ? l.label.slice(0, idx) : "(no namespace)";
      if (!buckets.has(ns)) buckets.set(ns, []);
      buckets.get(ns)!.push(l);
    }
    return [...buckets.entries()].sort(([a], [b]) => a.localeCompare(b));
  }, [filtered]);

  const total = labels?.length ?? 0;
  const totalUsages = labels?.reduce((acc, l) => acc + l.count, 0) ?? 0;

  const onApply = useCallback(
    async (op: () => Promise<unknown>, successMsg: string) => {
      setBusy(true);
      try {
        await op();
        await refresh();
        setDialog({ kind: "none" });
        if (typeof console !== "undefined") {
          console.info("[labels]", successMsg);
        }
      } catch (e) {
        setError((e as Error).message);
      } finally {
        setBusy(false);
      }
    },
    [refresh],
  );

  return (
    <div className="h-full overflow-auto p-4 space-y-3 text-[13px]">
      <header className="flex items-baseline gap-3">
        <h1 className="text-lg font-semibold text-fg-default">
          Board labels
        </h1>
        <span className="text-fg-muted text-[11px]">
          {total} distinct · {totalUsages} usage{totalUsages === 1 ? "" : "s"} total
        </span>
        <button
          type="button"
          onClick={() => setLocation("/board")}
          className="ml-auto text-fg-subtle hover:text-fg-default underline text-[11px]"
        >
          ← Back to board
        </button>
      </header>

      <p className="text-fg-muted text-[11px] max-w-3xl">
        Manage the vocabulary used across the native kanban. Rename a
        label to fix a typo, merge two labels that mean the same thing,
        or delete a label that no longer carries signal. Bots read this
        catalogue via{" "}
        <code className="text-[10px]">mcp__iterion_board__list_labels</code>{" "}
        before emitting new issues — keeping it tight directly
        constrains future runs.
      </p>

      <div className="flex items-center gap-2">
        <input
          type="text"
          placeholder="Filter by name…"
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          className="px-2 py-1 rounded border border-border-default bg-surface-0 text-fg-default text-[12px] w-56"
        />
        {searchQuery && (
          <button
            type="button"
            className="text-fg-subtle hover:text-fg-default text-[11px] underline"
            onClick={() => setSearchQuery("")}
          >
            clear
          </button>
        )}
      </div>

      {error && (
        <div className="text-danger-fg text-[11px]" role="alert">
          {error}
        </div>
      )}

      {!labels && <p className="text-fg-muted text-[11px]">Loading…</p>}

      {grouped && grouped.length === 0 && (
        <p className="text-fg-muted text-[11px] italic">
          {searchQuery
            ? "No labels match that filter."
            : "No labels on the board yet."}
        </p>
      )}

      {grouped &&
        grouped.map(([ns, rows]) => (
          <section key={ns} className="space-y-1">
            <h2 className="text-[11px] font-mono text-fg-muted uppercase tracking-wide">
              {ns}{" "}
              <span className="text-fg-subtle normal-case">
                · {rows.length} label{rows.length === 1 ? "" : "s"}
              </span>
            </h2>
            <table className="w-full text-[12px] border border-border-subtle">
              <thead>
                <tr className="bg-surface-1 text-fg-muted text-left">
                  <th className="px-2 py-1 font-medium">Label</th>
                  <th className="px-2 py-1 font-medium w-16 text-right">
                    Count
                  </th>
                  <th className="px-2 py-1 font-medium w-36">Last used</th>
                  <th className="px-2 py-1 font-medium w-64">Actions</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => (
                  <tr
                    key={row.label}
                    className="border-t border-border-subtle hover:bg-surface-1/40"
                  >
                    <td className="px-2 py-1 font-mono text-fg-default">
                      <button
                        type="button"
                        onClick={() =>
                          setLocation(
                            `/board?label=${encodeURIComponent(row.label)}`,
                          )
                        }
                        className="underline-offset-2 hover:underline text-left"
                        title="Filter the board by this label"
                      >
                        {row.label}
                      </button>
                    </td>
                    <td className="px-2 py-1 text-right tabular-nums text-fg-default">
                      {row.count}
                    </td>
                    <td className="px-2 py-1 text-fg-muted">
                      {row.last_used_at ? formatRelative(row.last_used_at) : "—"}
                    </td>
                    <td className="px-2 py-1">
                      <div className="flex gap-1.5">
                        <button
                          type="button"
                          className="text-fg-subtle hover:text-fg-default text-[11px] underline"
                          onClick={() =>
                            setDialog({
                              kind: "rename",
                              label: row,
                              nextValue: row.label,
                            })
                          }
                        >
                          rename
                        </button>
                        <button
                          type="button"
                          className="text-fg-subtle hover:text-fg-default text-[11px] underline"
                          onClick={() =>
                            setDialog({
                              kind: "merge",
                              label: row,
                              nextValue: "",
                            })
                          }
                        >
                          merge
                        </button>
                        <button
                          type="button"
                          className="text-danger-fg hover:text-danger text-[11px] underline"
                          onClick={() => setDialog({ kind: "delete", label: row })}
                        >
                          delete
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        ))}

      {dialog.kind === "rename" && (
        <LabelDialog
          title={`Rename ${dialog.label.label}`}
          description={`Rewrites the label on all ${dialog.label.count} issue${
            dialog.label.count === 1 ? "" : "s"
          } that carry it. Idempotent.`}
          submitLabel="Rename"
          inputLabel="New label name"
          inputValue={dialog.nextValue}
          onChange={(v) =>
            setDialog({ ...dialog, nextValue: v })
          }
          busy={busy}
          disabled={
            dialog.nextValue.trim() === "" ||
            dialog.nextValue === dialog.label.label
          }
          onCancel={() => setDialog({ kind: "none" })}
          onSubmit={() =>
            onApply(
              () => renameLabel(dialog.label.label, dialog.nextValue.trim()),
              `renamed ${dialog.label.label} → ${dialog.nextValue.trim()}`,
            )
          }
        />
      )}

      {dialog.kind === "merge" && (
        <LabelDialog
          title={`Merge ${dialog.label.label} into another label`}
          description={`Every issue carrying ${dialog.label.label} will also carry the target label (de-duped) and lose ${dialog.label.label}. Use this when two labels mean the same thing.`}
          submitLabel="Merge"
          inputLabel="Target label (must already exist on the board)"
          inputValue={dialog.nextValue}
          onChange={(v) => setDialog({ ...dialog, nextValue: v })}
          autocomplete={labels?.map((l) => l.label) ?? []}
          busy={busy}
          disabled={
            dialog.nextValue.trim() === "" ||
            dialog.nextValue.trim() === dialog.label.label
          }
          onCancel={() => setDialog({ kind: "none" })}
          onSubmit={() =>
            onApply(
              () => mergeLabels(dialog.label.label, dialog.nextValue.trim()),
              `merged ${dialog.label.label} into ${dialog.nextValue.trim()}`,
            )
          }
        />
      )}

      {dialog.kind === "delete" && (
        <ConfirmDialog
          title={`Delete ${dialog.label.label}?`}
          description={`Strips ${dialog.label.label} from all ${dialog.label.count} issue${
            dialog.label.count === 1 ? "" : "s"
          } that carry it. The issues themselves are kept. This cannot be undone — but you can re-add the label per-issue later.`}
          confirmLabel="Delete"
          busy={busy}
          onCancel={() => setDialog({ kind: "none" })}
          onConfirm={() =>
            onApply(
              () => deleteLabel(dialog.label.label),
              `deleted ${dialog.label.label}`,
            )
          }
        />
      )}
    </div>
  );
}

interface LabelDialogProps {
  title: string;
  description: string;
  submitLabel: string;
  inputLabel: string;
  inputValue: string;
  onChange: (v: string) => void;
  autocomplete?: string[];
  busy: boolean;
  disabled: boolean;
  onCancel: () => void;
  onSubmit: () => void;
}

function LabelDialog({
  title,
  description,
  submitLabel,
  inputLabel,
  inputValue,
  onChange,
  autocomplete,
  busy,
  disabled,
  onCancel,
  onSubmit,
}: LabelDialogProps) {
  return (
    <ModalShell onCancel={onCancel}>
      <h3 className="text-[14px] font-medium text-fg-default">{title}</h3>
      <p className="text-[12px] text-fg-muted">{description}</p>
      <label className="block space-y-1">
        <span className="text-[11px] text-fg-muted">{inputLabel}</span>
        <input
          type="text"
          autoFocus
          list={autocomplete ? "labels-autocomplete" : undefined}
          value={inputValue}
          onChange={(e) => onChange(e.target.value)}
          className="w-full px-2 py-1 rounded border border-border-default bg-surface-0 text-fg-default text-[12px] font-mono"
        />
        {autocomplete && (
          <datalist id="labels-autocomplete">
            {autocomplete.map((l) => (
              <option key={l} value={l} />
            ))}
          </datalist>
        )}
      </label>
      <ModalActions
        onCancel={onCancel}
        primaryLabel={submitLabel}
        primaryVariant="primary"
        onPrimary={onSubmit}
        busy={busy}
        disabled={disabled}
      />
    </ModalShell>
  );
}

interface ConfirmDialogProps {
  title: string;
  description: string;
  confirmLabel: string;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}

function ConfirmDialog({
  title,
  description,
  confirmLabel,
  busy,
  onCancel,
  onConfirm,
}: ConfirmDialogProps) {
  return (
    <ModalShell onCancel={onCancel}>
      <h3 className="text-[14px] font-medium text-fg-default">{title}</h3>
      <p className="text-[12px] text-fg-muted">{description}</p>
      <ModalActions
        onCancel={onCancel}
        primaryLabel={confirmLabel}
        primaryVariant="danger"
        onPrimary={onConfirm}
        busy={busy}
      />
    </ModalShell>
  );
}

function ModalShell({
  children,
  onCancel,
}: {
  children: React.ReactNode;
  onCancel: () => void;
}) {
  // Block-level modal pinned to the viewport via fixed positioning;
  // we don't pull in a portal library for one screen. Escape closes;
  // click-outside closes (the backdrop's onClick).
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, [onCancel]);
  return (
    <div
      className="fixed inset-0 z-50 bg-surface-overlay/60 flex items-center justify-center p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget) onCancel();
      }}
    >
      <div className="bg-surface-0 border border-border-default rounded-md shadow-lg p-4 w-full max-w-md space-y-3">
        {children}
      </div>
    </div>
  );
}

function ModalActions({
  onCancel,
  primaryLabel,
  primaryVariant,
  onPrimary,
  busy,
  disabled,
}: {
  onCancel: () => void;
  primaryLabel: string;
  primaryVariant: "primary" | "danger";
  onPrimary: () => void;
  busy: boolean;
  disabled?: boolean;
}) {
  return (
    <div className="flex justify-end gap-2 pt-1">
      <Button variant="secondary" size="sm" onClick={onCancel} disabled={busy}>
        Cancel
      </Button>
      <Button
        variant={primaryVariant}
        size="sm"
        onClick={onPrimary}
        loading={busy}
        disabled={busy || disabled}
      >
        {busy ? "…" : primaryLabel}
      </Button>
    </div>
  );
}
