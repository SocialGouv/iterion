import * as RP from "@radix-ui/react-popover";
import {
  useCallback,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

export interface ComboboxOption<T = string> {
  /** Stable value used for selection comparison and `onChange`. */
  value: T;
  /** Primary line shown in the input + popup row. */
  label: string;
  /** Secondary line (subtitle) under the label in the popup row. */
  description?: string;
  /** Optional right-side metadata (tags/chips). */
  meta?: ReactNode;
  /** Search text — concatenated with label + description when filtering. */
  searchHaystack?: string;
}

interface Props<T = string> {
  value: T | "";
  options: ComboboxOption<T>[];
  /** Empty-value affordance shown at the top of the popup. */
  emptyLabel?: string;
  emptyDescription?: string;
  placeholder?: string;
  onChange: (value: T | "") => void;
  disabled?: boolean;
  size?: "sm" | "md";
  /** Stable id for aria-controls when the host wants to label it
   *  externally. */
  id?: string;
  /** Allow committing the typed text as a new value (not in `options`).
   *  Adds a "Use '<query>'" row + Enter-commits-on-no-match. Only
   *  meaningful when T is string-like (e.g. a free-form assignee). */
  freeSolo?: boolean;
}

/** Combobox is a searchable single-select primitive. The Board ticket
 *  form's bot picker is its first consumer; design generic enough to
 *  reuse for other "pick one from a small typed list" cases (recipe
 *  selector, backend override, …).
 *
 *  Keyboard:
 *    - typing filters the popup; Down / Up move focus; Enter selects.
 *    - Escape closes without changing the value.
 *    - clicking outside closes.
 */
export function Combobox<T = string>({
  value,
  options,
  emptyLabel,
  emptyDescription,
  placeholder = "Search…",
  onChange,
  disabled = false,
  size = "sm",
  id,
  freeSolo = false,
}: Props<T>) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [focusIdx, setFocusIdx] = useState(-1);
  const inputRef = useRef<HTMLInputElement | null>(null);

  const selected = useMemo(
    () => options.find((o) => o.value === value),
    [options, value],
  );

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return options;
    return options.filter((o) => {
      const hay = (
        o.searchHaystack ??
        `${o.label} ${o.description ?? ""}`
      ).toLowerCase();
      return hay.includes(q);
    });
  }, [options, query]);

  const trimmedQuery = query.trim();
  // Free-text affordance: offer the typed value when it isn't already an
  // option (and freeSolo is on).
  const showFreeSolo =
    freeSolo &&
    trimmedQuery.length > 0 &&
    !options.some((o) => String(o.value) === trimmedQuery);

  const close = useCallback(() => {
    setOpen(false);
    setQuery("");
    setFocusIdx(-1);
  }, []);

  const sizeClass = size === "sm" ? "h-7 text-xs px-2" : "h-9 text-sm px-2.5";

  const commitIdx = (idx: number) => {
    // -1 = the "(emptyLabel)" row at the top of the popup when emptyLabel is set
    if (emptyLabel != null && idx === -1) {
      onChange("");
      close();
      return;
    }
    const opt = filtered[idx];
    if (!opt) return;
    onChange(opt.value);
    close();
  };

  const commitFreeText = () => {
    if (!trimmedQuery) return;
    onChange(trimmedQuery as T);
    close();
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open) {
      if (e.key === "ArrowDown" || e.key === "Enter") {
        e.preventDefault();
        setOpen(true);
      }
      return;
    }
    const total = filtered.length + (emptyLabel != null ? 1 : 0);
    const min = emptyLabel != null ? -1 : 0;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setFocusIdx((p) => {
        if (p < 0 && emptyLabel != null) return 0;
        return Math.min(filtered.length - 1, p + 1);
      });
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setFocusIdx((p) => Math.max(min, p - 1));
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (showFreeSolo && filtered.length === 0) commitFreeText();
      else commitIdx(focusIdx >= 0 ? focusIdx : 0);
    } else if (e.key === "Escape") {
      e.preventDefault();
      close();
    }
    void total;
  };

  // Header shows the selected option's label or the placeholder.
  const headerText = selected
    ? selected.label
    : value === "" && emptyLabel
      ? emptyLabel
      : "";

  const listboxId = useId();

  // The popup rides a portaled Radix Popover instead of an absolutely
  // positioned sibling: inside a Dialog body (overflow-y-auto) the old
  // popup was clipped to the scroll container — ~1.5 visible rows in the
  // AddTaskDialog. Radix portals it above the dialog, flips when space
  // below runs out, and hands us the real available height via CSS var.
  return (
    <RP.Root open={open} onOpenChange={(o) => (o ? setOpen(true) : close())}>
      <RP.Trigger asChild>
        <button
          type="button"
          id={id}
          disabled={disabled}
          className={`w-full text-left bg-surface-1 text-fg-default rounded-md border border-border-strong outline-none transition-colors disabled:opacity-60 disabled:cursor-not-allowed focus:border-accent focus:ring-1 focus:ring-accent ${sizeClass} flex items-center justify-between gap-2`}
          aria-haspopup="listbox"
          aria-controls={open ? listboxId : undefined}
        >
          <span className={headerText ? "" : "text-fg-subtle"}>
            {headerText || placeholder}
          </span>
          <span className="text-fg-subtle text-caption">▾</span>
        </button>
      </RP.Trigger>
      <RP.Portal>
        <RP.Content
          role="listbox"
          id={listboxId}
          align="start"
          sideOffset={4}
          // Focus the search input, not the popup container.
          onOpenAutoFocus={(e) => {
            e.preventDefault();
            inputRef.current?.focus();
          }}
          className="z-[var(--z-popover)] w-[var(--radix-popover-trigger-width)] max-h-[min(18rem,var(--radix-popover-content-available-height))] overflow-auto rounded-md border border-border-strong bg-surface-0 shadow-[var(--shadow-popover)]"
        >
          <div className="p-1 sticky top-0 bg-surface-0 border-b border-border-default">
            <input
              ref={inputRef}
              type="text"
              value={query}
              placeholder={placeholder}
              onChange={(e) => {
                setQuery(e.target.value);
                setFocusIdx(emptyLabel != null ? -1 : 0);
              }}
              onKeyDown={onKeyDown}
              className="w-full bg-surface-1 text-fg-default rounded border border-border-default px-2 py-1 text-xs outline-none focus:border-accent"
            />
          </div>
          <ul className="py-1">
            {emptyLabel != null && (
              <li
                role="option"
                aria-selected={value === ""}
                onMouseEnter={() => setFocusIdx(-1)}
                onMouseDown={(e) => {
                  e.preventDefault();
                  commitIdx(-1);
                }}
                className={`px-2 py-1.5 cursor-pointer text-xs ${
                  focusIdx === -1 ? "bg-surface-2" : "hover:bg-surface-1"
                } ${value === "" ? "text-accent-text" : "text-fg-muted italic"}`}
              >
                {emptyLabel}
                {emptyDescription && (
                  <div className="text-caption text-fg-subtle italic mt-0.5">
                    {emptyDescription}
                  </div>
                )}
              </li>
            )}
            {filtered.length === 0 && !showFreeSolo && (
              <li className="px-2 py-2 text-xs text-fg-subtle italic">
                No matches
              </li>
            )}
            {filtered.map((o, idx) => {
              const isFocused = idx === focusIdx;
              const isSelected = o.value === value;
              return (
                <li
                  key={String(o.value)}
                  role="option"
                  aria-selected={isSelected}
                  onMouseEnter={() => setFocusIdx(idx)}
                  onMouseDown={(e) => {
                    e.preventDefault();
                    commitIdx(idx);
                  }}
                  className={`px-2 py-1.5 cursor-pointer ${
                    isFocused ? "bg-surface-2" : "hover:bg-surface-1"
                  } ${isSelected ? "border-l-2 border-accent pl-[6px]" : ""}`}
                >
                  <div className="flex items-center justify-between gap-2">
                    <span className="text-xs font-mono text-fg-default truncate">
                      {o.label}
                    </span>
                    {o.meta && (
                      <span className="text-caption text-fg-subtle shrink-0">
                        {o.meta}
                      </span>
                    )}
                  </div>
                  {o.description && (
                    <div className="text-caption text-fg-muted truncate mt-0.5">
                      {o.description}
                    </div>
                  )}
                </li>
              );
            })}
            {showFreeSolo && (
              <li
                role="option"
                aria-selected={false}
                onMouseDown={(e) => {
                  e.preventDefault();
                  commitFreeText();
                }}
                className="px-2 py-1.5 cursor-pointer text-xs text-fg-muted hover:bg-surface-1"
              >
                Use “{trimmedQuery}”
              </li>
            )}
          </ul>
        </RP.Content>
      </RP.Portal>
    </RP.Root>
  );
}
