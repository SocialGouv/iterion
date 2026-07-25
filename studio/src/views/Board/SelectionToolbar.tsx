import { useEffect, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Tooltip } from "@/components/ui";
import { useServerInfoStore } from "@/store/serverInfo";
import type { NativeBoard, NativeIssue } from "@/api/native";

import { PRIORITY_PRESETS } from "./boardShared";

// SelectionToolbar is the action bar shown whenever ≥1 card is selected.
// It hosts the bulk operations (dispatch, move, priority, assignee,
// label, delete) so triage is possible without opening each card.
export function SelectionToolbar({
  count,
  board,
  allLabels,
  allAssignees,
  selectedIssues,
  allSelectedDispatchable,
  onDispatch,
  onMove,
  onPriority,
  onAssignee,
  onToggleLabel,
  onDelete,
  onClear,
}: {
  count: number;
  board: NativeBoard;
  allLabels: string[];
  allAssignees: string[];
  selectedIssues: NativeIssue[];
  allSelectedDispatchable: boolean;
  onDispatch: () => void;
  onMove: (state: string) => void;
  onPriority: (p: number) => void;
  onAssignee: (a: string) => void;
  onToggleLabel: (label: string) => void;
  onDelete: () => void;
  onClear: () => void;
}) {
  // Without a dispatcher the eligible column is the staging lane (cards
  // launch their assigned bot on arrival), so the tooltip names it that way.
  const dispatcherEnabled = useServerInfoStore(
    (s) => s.info?.dispatcher_enabled === true,
  );
  return (
    <div className="shrink-0 px-3 py-1.5 border-b border-border-default bg-accent-soft flex flex-wrap items-center gap-2 text-xs text-fg-default">
      <span>
        <strong>{count}</strong> selected
      </span>
      <Tooltip
        content={
          allSelectedDispatchable
            ? `Move all selected into the ${dispatcherEnabled ? "dispatch" : "staging"} lane`
            : "All selected cards must be in Inbox or Backlog"
        }
      >
        <Button
          variant="success"
          size="sm"
          onClick={onDispatch}
          disabled={!allSelectedDispatchable}
        >
          ▶ Let's go
        </Button>
      </Tooltip>

      <div className="w-auto">
        <Select
          value=""
          onChange={(e) => {
            if (e.target.value) onMove(e.target.value);
          }}
          aria-label="Bulk move to column"
        >
          <option value="">Move to…</option>
          {board.states.map((s) => (
            <option key={s.name} value={s.name}>
              {s.display ?? s.name}
            </option>
          ))}
        </Select>
      </div>

      <div className="w-auto">
        <Select
          value=""
          onChange={(e) => {
            if (e.target.value !== "") onPriority(Number(e.target.value));
          }}
          aria-label="Bulk set priority"
        >
          <option value="">Priority…</option>
          {PRIORITY_PRESETS.map((p) => (
            <option key={p} value={p}>
              P{p}
            </option>
          ))}
        </Select>
      </div>

      <div className="w-auto">
        <Select
          value=""
          onChange={(e) => {
            const v = e.target.value;
            if (v === "") return;
            onAssignee(v === "__clear__" ? "" : v);
          }}
          aria-label="Bulk set assignee"
        >
          <option value="">Assignee…</option>
          <option value="__clear__">(clear)</option>
          {allAssignees.map((a) => (
            <option key={a} value={a}>
              @{a}
            </option>
          ))}
        </Select>
      </div>

      <BulkLabelPopover
        allLabels={allLabels}
        selectedIssues={selectedIssues}
        onToggle={onToggleLabel}
      />

      <div className="ml-auto flex items-center gap-2">
        <Button variant="danger" size="sm" onClick={onDelete}>
          Delete
        </Button>
        <Button variant="ghost" size="sm" onClick={onClear}>
          clear
        </Button>
      </div>
    </div>
  );
}

// BulkLabelPopover toggles a label across the whole selection. Each row
// is tri-state: ✓ when every selected issue has the label, – when some
// do, empty when none. Clicking adds it to all (or removes from all when
// every selected issue already has it). Stays open for rapid tagging.
function BulkLabelPopover({
  allLabels,
  selectedIssues,
  onToggle,
}: {
  allLabels: string[];
  selectedIssues: NativeIssue[];
  onToggle: (label: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const rootRef = useRef<HTMLDivElement | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
        setQuery("");
      }
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? allLabels.filter((l) => l.toLowerCase().includes(q)) : allLabels;
  }, [allLabels, query]);
  const total = selectedIssues.length;
  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => {
          setOpen((o) => !o);
          setTimeout(() => inputRef.current?.focus(), 0);
        }}
        className="px-2 py-0.5 rounded border border-border-default bg-surface-0 text-fg-muted hover:text-fg-default flex items-center gap-1"
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        Label <span className="text-fg-subtle text-caption">▾</span>
      </button>
      {open && (
        <div className="absolute z-[var(--z-popover)] mt-1 w-64 max-h-80 overflow-hidden rounded-md border border-border-strong bg-surface-0 shadow-popover flex flex-col">
          <div className="p-1 border-b border-border-default shrink-0">
            <Input
              ref={inputRef}
              type="text"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search labels…"
              aria-label="Search labels"
            />
          </div>
          <ul className="py-1 overflow-auto">
            {filtered.length === 0 && (
              <li className="px-2 py-2 text-xs text-fg-subtle italic">No matches</li>
            )}
            {filtered.map((l) => {
              const c = selectedIssues.reduce(
                (n, i) => n + ((i.labels ?? []).includes(l) ? 1 : 0),
                0,
              );
              const mark = c === 0 ? "" : c === total ? "✓" : "–";
              const active = c > 0;
              return (
                <li key={l}>
                  <button
                    type="button"
                    onClick={() => onToggle(l)}
                    className={`w-full text-left px-2 py-1.5 text-xs flex items-center gap-2 hover:bg-surface-1 rounded focus:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
                      active ? "text-fg-default" : "text-fg-muted"
                    }`}
                  >
                    <span
                      className={`inline-flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded border text-[9px] ${
                        active
                          ? "bg-accent border-accent text-fg-onAccent"
                          : "border-border-strong"
                      }`}
                    >
                      {mark}
                    </span>
                    <span className="truncate">{l}</span>
                  </button>
                </li>
              );
            })}
          </ul>
        </div>
      )}
    </div>
  );
}
