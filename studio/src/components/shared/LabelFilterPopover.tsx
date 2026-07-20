import { useEffect, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";

// LabelFilter is a searchable multi-select popover over a label vocabulary,
// shared by the /board backlog filters and the /pipelines board filters.
// It replaces a flat chip strip, which grows unwieldy once boards accumulate
// dozens of labels. Selection is the caller's Set; toggling stays in sync
// with any other surface bound to the same Set.
export function LabelFilter({
  allLabels,
  selected,
  onToggle,
  onClear,
  label = "Labels",
  searchPlaceholder = "Search labels…",
}: {
  allLabels: string[];
  selected: Set<string>;
  onToggle: (l: string) => void;
  onClear: () => void;
  /** Button caption (e.g. "Tags" on the pipeline board). */
  label?: string;
  searchPlaceholder?: string;
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

  const count = selected.size;

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => {
          setOpen((o) => !o);
          setTimeout(() => inputRef.current?.focus(), 0);
        }}
        className={`px-2 py-1 rounded border flex items-center gap-1 ${
          count > 0
            ? "border-accent text-fg-default bg-accent-soft/30"
            : "border-border-default text-fg-muted hover:text-fg-default bg-surface-0"
        }`}
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span>{label}</span>
        {count > 0 && (
          <span className="px-1 rounded bg-accent text-fg-onAccent text-caption">{count}</span>
        )}
        <span className="text-fg-subtle text-caption">▾</span>
      </button>

      {open && (
        <div className="absolute z-[var(--z-popover)] mt-1 w-64 max-h-80 overflow-hidden rounded-md border border-border-strong bg-surface-0 shadow-popover flex flex-col">
          <div className="p-1 border-b border-border-default shrink-0">
            <Input
              ref={inputRef}
              type="text"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={searchPlaceholder}
              aria-label={searchPlaceholder}
            />
          </div>
          <ul className="py-1 overflow-auto">
            {filtered.length === 0 && (
              <li className="px-2 py-2 text-xs text-fg-subtle italic">No matches</li>
            )}
            {filtered.map((l) => {
              const active = selected.has(l);
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
                      {active ? "✓" : ""}
                    </span>
                    <span className="truncate">{l}</span>
                  </button>
                </li>
              );
            })}
          </ul>
          {count > 0 && (
            <div className="p-1 border-t border-border-default shrink-0">
              <Button
                variant="ghost"
                size="sm"
                onClick={onClear}
                className="w-full justify-center"
              >
                Clear {count} label{count > 1 ? "s" : ""}
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
