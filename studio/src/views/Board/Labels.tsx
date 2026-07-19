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

import { useCallback, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation } from "wouter";

import { useHeaderSlot } from "@/components/shared/useHeaderSlot";

import {
  deleteLabel,
  listLabels,
  mergeLabels,
  renameLabel,
  type LabelUsage,
} from "@/api/native";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";

type DialogState =
  | { kind: "none" }
  | { kind: "rename"; label: LabelUsage; nextValue: string }
  | { kind: "merge"; label: LabelUsage; nextValue: string };

export default function LabelsView() {
  return (
    <ErrorBoundary area="Labels view">
      <LabelsViewInner />
    </ErrorBoundary>
  );
}

function LabelsViewInner() {
  const [, setLocation] = useLocation();
  const [searchQuery, setSearchQuery] = useState("");
  const [dialog, setDialog] = useState<DialogState>({ kind: "none" });
  const action = useAsyncAction();
  const { confirm, dialog: confirmDialog } = useConfirm();

  const queryClient = useQueryClient();
  const labelsQuery = useQuery<LabelUsage[]>({
    queryKey: ["board-labels"],
    queryFn: () => listLabels(),
  });
  const labels = labelsQuery.data ?? null;
  const loadError = labelsQuery.error ? errorMessage(labelsQuery.error) : null;

  // Label ops rewrite the catalogue — re-pull it after each write.
  const refresh = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["board-labels"] }),
    [queryClient],
  );

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
    async (op: () => Promise<unknown>, _successMsg: string) => {
      const ok = await action.run(async () => {
        await op();
        await refresh();
        return true;
      });
      if (ok) setDialog({ kind: "none" });
    },
    [action, refresh],
  );

  const onDelete = useCallback(
    async (row: LabelUsage) => {
      const ok = await confirm({
        title: `Delete ${row.label}?`,
        message: `Strips ${row.label} from all ${row.count} issue${
          row.count === 1 ? "" : "s"
        } that carry it. The issues themselves are kept. This cannot be undone — but you can re-add the label per-issue later.`,
        confirmLabel: "Delete",
        confirmVariant: "danger",
      });
      if (!ok) return;
      await action.run(async () => {
        await deleteLabel(row.label);
        await refresh();
      });
    },
    [action, confirm, refresh],
  );

  useHeaderSlot({
    left: (
      <span className="flex items-center gap-1.5 text-xs font-medium text-fg-default">
        <Link href="/board" className="text-fg-muted hover:text-fg-default hover:underline">
          Board
        </Link>
        <span className="text-fg-subtle">/</span>
        <span>Labels</span>
      </span>
    ),
  });

  return (
    <div className="h-full overflow-auto p-4 space-y-3 text-label">
      <header className="flex items-baseline gap-3">
        <h1 className="text-headline font-semibold tracking-tight text-fg-default">
          Board labels
        </h1>
        <span className="text-fg-muted text-micro">
          {total} distinct · {totalUsages} usage{totalUsages === 1 ? "" : "s"} total
        </span>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setLocation("/board")}
          className="ml-auto"
        >
          ← Back to board
        </Button>
      </header>

      <p className="text-fg-muted text-micro max-w-3xl">
        Manage the vocabulary used across the native kanban. Rename a
        label to fix a typo, merge two labels that mean the same thing,
        or delete a label that no longer carries signal. Bots read this
        catalogue via{" "}
        <code className="text-caption">mcp__iterion_board__list_labels</code>{" "}
        before emitting new issues — keeping it tight directly
        constrains future runs.
      </p>

      <div className="flex items-center gap-2">
        <div className="w-56">
          <Input
            type="text"
            placeholder="Filter by name…"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            aria-label="Filter labels by name"
          />
        </div>
        {searchQuery && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setSearchQuery("")}
          >
            clear
          </Button>
        )}
      </div>

      {(action.error || loadError) && (
        <div className="text-danger-fg text-micro" role="alert">
          {action.error ?? loadError}
        </div>
      )}

      {!labels && <TableSkeleton />}

      {grouped && grouped.length === 0 && (
        searchQuery ? (
          <EmptyState message="No labels match that filter." />
        ) : (
          <EmptyState
            title="No labels yet"
            message="Labels appear here once issues on the board carry them — bots and operators add them as they triage. Namespaced labels (source:, horizon:, …) group automatically."
          />
        )
      )}

      {grouped &&
        grouped.map(([ns, rows]) => (
          <section key={ns} className="space-y-1">
            <h2 className="text-micro font-mono text-fg-muted uppercase tracking-wide">
              {ns}{" "}
              <span className="text-fg-subtle normal-case">
                · {rows.length} label{rows.length === 1 ? "" : "s"}
              </span>
            </h2>
            <Table
              caption={`Labels in the ${ns} namespace`}
              className="border border-border-subtle"
            >
              <THead className="bg-surface-1">
                <Th>Label</Th>
                <Th align="right" className="w-16">
                  Count
                </Th>
                <Th className="w-36">Last used</Th>
                <Th className="w-64">Actions</Th>
              </THead>
              <TBody>
                {rows.map((row) => (
                  <Tr key={row.label}>
                    <Td className="font-mono text-fg-default">
                      <button
                        type="button"
                        onClick={() =>
                          setLocation(
                            `/board?label=${encodeURIComponent(row.label)}`,
                          )
                        }
                        className="underline-offset-2 hover:underline text-left rounded focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
                        title="Filter the board by this label"
                      >
                        {row.label}
                      </button>
                    </Td>
                    <Td align="right" className="tabular-nums text-fg-default">
                      {row.count}
                    </Td>
                    <Td className="text-fg-muted">
                      {row.last_used_at ? formatRelative(row.last_used_at) : "—"}
                    </Td>
                    <Td>
                      <div className="flex gap-1.5">
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() =>
                            setDialog({
                              kind: "rename",
                              label: row,
                              nextValue: row.label,
                            })
                          }
                        >
                          rename
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() =>
                            setDialog({
                              kind: "merge",
                              label: row,
                              nextValue: "",
                            })
                          }
                        >
                          merge
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-danger-fg hover:text-danger"
                          onClick={() => void onDelete(row)}
                        >
                          delete
                        </Button>
                      </div>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
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
          busy={action.busy}
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
          busy={action.busy}
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

      {confirmDialog}
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
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onCancel();
      }}
      title={title}
      description={description}
      widthClass="max-w-md"
      footer={
        <ModalActions
          onCancel={onCancel}
          primaryLabel={submitLabel}
          primaryVariant="primary"
          onPrimary={onSubmit}
          busy={busy}
          disabled={disabled}
        />
      }
    >
      <label className="block space-y-1">
        <span className="text-micro text-fg-muted">{inputLabel}</span>
        <Input
          type="text"
          autoFocus
          list={autocomplete ? "labels-autocomplete" : undefined}
          value={inputValue}
          onChange={(e) => onChange(e.target.value)}
          className="font-mono"
        />
        {autocomplete && (
          <datalist id="labels-autocomplete">
            {autocomplete.map((l) => (
              <option key={l} value={l} />
            ))}
          </datalist>
        )}
      </label>
    </Dialog>
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
    <>
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
    </>
  );
}
