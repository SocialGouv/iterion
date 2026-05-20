import {
  useCallback,
  useEffect,
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
}: Props<T>) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [focusIdx, setFocusIdx] = useState(-1);
  const rootRef = useRef<HTMLDivElement | null>(null);
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

  const close = useCallback(() => {
    setOpen(false);
    setQuery("");
    setFocusIdx(-1);
  }, []);

  useEffect(() => {
    if (!open) return;
    function onDoc(ev: MouseEvent) {
      if (!rootRef.current) return;
      if (!rootRef.current.contains(ev.target as Node)) close();
    }
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open, close]);

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
      commitIdx(focusIdx >= 0 ? focusIdx : 0);
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

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        id={id}
        disabled={disabled}
        onClick={() => {
          if (disabled) return;
          setOpen((o) => !o);
          setTimeout(() => inputRef.current?.focus(), 0);
        }}
        className={`w-full text-left bg-surface-1 text-fg-default rounded-md border border-border-strong outline-none transition-colors disabled:opacity-60 disabled:cursor-not-allowed focus:border-accent focus:ring-1 focus:ring-accent ${sizeClass} flex items-center justify-between gap-2`}
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span className={headerText ? "" : "text-fg-subtle"}>
          {headerText || placeholder}
        </span>
        <span className="text-fg-subtle text-[10px]">▾</span>
      </button>

      {open && (
        <div
          role="listbox"
          className="absolute z-30 mt-1 w-full max-h-72 overflow-auto rounded-md border border-border-strong bg-surface-0 shadow-lg"
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
                } ${value === "" ? "text-accent" : "text-fg-muted italic"}`}
              >
                {emptyLabel}
                {emptyDescription && (
                  <div className="text-[10px] text-fg-subtle italic mt-0.5">
                    {emptyDescription}
                  </div>
                )}
              </li>
            )}
            {filtered.length === 0 && (
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
                      <span className="text-[10px] text-fg-subtle shrink-0">
                        {o.meta}
                      </span>
                    )}
                  </div>
                  {o.description && (
                    <div className="text-[10px] text-fg-muted truncate mt-0.5">
                      {o.description}
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        </div>
      )}
    </div>
  );
}
