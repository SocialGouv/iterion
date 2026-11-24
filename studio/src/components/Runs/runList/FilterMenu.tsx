import { useState, type ReactNode } from "react";

import { CheckIcon, ChevronDownIcon } from "@radix-ui/react-icons";

import { Popover } from "@/components/ui/Popover";

// One option in a FilterMenu. `key` is the filter value ("" = the
// unset/"All" entry); `label` is a (possibly icon/emoji-laden) node;
// `count` is an optional right-aligned tally.
export interface FilterMenuOption {
  key: string;
  label: ReactNode;
  count?: number;
}

// FilterMenu is the per-axis dropdown pill used by the Status, Source,
// Bot, Repo/Folder, and Since filters. The trigger shows the axis name
// and, when a non-default value is selected, the active option's label
// (and an accent highlight). The popover lists the options with a check
// on the active one. Selecting closes the menu.
//
// The trigger's accessible name leads with the axis ("Since: Filter by
// date") so it CONTAINS the visible label (RGAA 7.1) — pass `ariaLabel`
// without repeating the axis.
export function FilterMenu({
  axis,
  ariaLabel,
  value,
  defaultValue = "",
  options,
  onSelect,
}: {
  axis: string;
  ariaLabel: string;
  value: string;
  defaultValue?: string;
  options: FilterMenuOption[];
  onSelect: (key: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const active = value !== defaultValue;
  const activeOption = active ? options.find((o) => o.key === value) : undefined;
  const triggerCls = active
    ? "border-accent/40 bg-accent-soft text-fg-default"
    : "border-border-default bg-surface-2 text-fg-muted hover:bg-surface-3";
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      contentClassName="p-1"
      trigger={
        <button
          type="button"
          aria-label={`${axis}: ${ariaLabel}`}
          className={`inline-flex items-center gap-1 rounded-md border text-xs h-7 px-2 ${triggerCls}`}
        >
          <span>{axis}</span>
          {activeOption && (
            <span className="flex items-center gap-1 max-w-40 truncate text-fg-default">
              <span className="text-fg-subtle">:</span>
              {activeOption.label}
            </span>
          )}
          <ChevronDownIcon className="w-3 h-3 opacity-60" />
        </button>
      }
    >
      <div role="listbox" aria-label={ariaLabel} className="min-w-44 max-h-72 overflow-auto">
        {options.map((o) => {
          const selected = o.key === value;
          return (
            <button
              key={o.key || "__all"}
              type="button"
              role="option"
              aria-selected={selected}
              onClick={() => {
                onSelect(o.key);
                setOpen(false);
              }}
              className={`w-full flex items-center gap-2 rounded px-2 h-7 text-xs text-left ${
                selected
                  ? "bg-accent-soft text-fg-default"
                  : "text-fg-default hover:bg-surface-2"
              }`}
            >
              <CheckIcon
                className={`w-3 h-3 shrink-0 text-accent-text ${selected ? "opacity-100" : "opacity-0"}`}
              />
              <span className="flex-1 truncate">{o.label}</span>
              {o.count !== undefined && (
                <span className="text-fg-muted tabular-nums">{o.count}</span>
              )}
            </button>
          );
        })}
      </div>
    </Popover>
  );
}
