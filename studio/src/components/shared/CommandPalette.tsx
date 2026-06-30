import { useEffect, useMemo, useRef, useState } from "react";
import { MagnifyingGlassIcon } from "@radix-ui/react-icons";

import { Kbd } from "@/components/ui/Kbd";

// Footer keyboard legend — the signature ⌘K affordance. Declarative so the
// hints stay in one place.
const PALETTE_HINTS: { keys: string[]; label: string }[] = [
  { keys: ["↑", "↓"], label: "navigate" },
  { keys: ["↵"], label: "select" },
  { keys: ["esc"], label: "close" },
];

export interface CommandAction {
  id: string;
  title: string;
  // group is shown as a small label in the row — "File", "Edit", "Navigate".
  group: string;
  // keywords expand the fuzzy-search surface beyond the title. Use this
  // for shortcut hints ("ctrl+s"), synonyms ("save", "write"), or
  // capability tags ("new", "blank").
  keywords?: string[];
  shortcut?: string;
  run: () => void;
  // When false, the action is rendered greyed out and is unselectable.
  disabled?: boolean;
}

interface Props {
  open: boolean;
  actions: CommandAction[];
  onClose: () => void;
}

// CommandPalette renders a Cmd+K / Ctrl+K spotlight over the studio.
// Filtering is intentionally minimal — title + keywords + group split
// into tokens and matched substring-wise. That's good enough for ~50
// actions and avoids pulling in a fuzzy-search dep.
export default function CommandPalette({ open, actions, onClose }: Props) {
  const [query, setQuery] = useState("");
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const restoreFocusRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    if (open) {
      // Remember what had focus so it can be restored on close — focus
      // must not fall to <body> when a modal dismisses (WCAG 2.4.3).
      restoreFocusRef.current = (document.activeElement as HTMLElement) ?? null;
      setQuery("");
      setCursor(0);
      // setTimeout puts the focus call after the dialog mount so the
      // input receives the focus reliably across React 19 + Radix.
      const id = setTimeout(() => inputRef.current?.focus(), 10);
      return () => {
        clearTimeout(id);
        restoreFocusRef.current?.focus?.();
      };
    }
    return;
  }, [open]);

  const matches = useMemo(() => {
    const enabled = actions.filter((a) => !a.disabled);
    const q = query.trim().toLowerCase();
    if (!q) return enabled;
    const tokens = q.split(/\s+/).filter(Boolean);
    return enabled.filter((a) => {
      const hay = (
        a.title +
        " " +
        a.group +
        " " +
        (a.keywords?.join(" ") ?? "") +
        " " +
        (a.shortcut ?? "")
      ).toLowerCase();
      return tokens.every((t) => hay.includes(t));
    });
  }, [actions, query]);

  // Clamp cursor whenever the match set shrinks under it.
  useEffect(() => {
    if (cursor >= matches.length) setCursor(0);
  }, [matches.length, cursor]);

  if (!open) return null;

  const handleKey = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onClose();
      return;
    }
    if (e.key === "Tab") {
      // Modal trap: keep focus on the input (combobox pattern — the list
      // is navigated with arrows + aria-activedescendant, not Tab) so
      // focus never escapes to the page behind the overlay.
      e.preventDefault();
      return;
    }
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setCursor((c) => Math.min(matches.length - 1, c + 1));
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      setCursor((c) => Math.max(0, c - 1));
      return;
    }
    if (e.key === "Enter") {
      e.preventDefault();
      const target = matches[cursor];
      if (target) {
        onClose();
        target.run();
      }
      return;
    }
  };

  return (
    <div
      className="fixed inset-0 z-[var(--z-confirm)] bg-scrim-popover flex items-start justify-center pt-[15vh]"
      onClick={onClose}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Command palette"
        className="w-[min(560px,calc(100vw-2rem))] rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-lg)] overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-2.5 px-4 border-b border-border-default">
          <MagnifyingGlassIcon className="h-4 w-4 shrink-0 text-fg-subtle" aria-hidden />
          <input
            ref={inputRef}
            type="text"
            role="combobox"
            aria-expanded={matches.length > 0}
            aria-controls="cmdk-listbox"
            aria-activedescendant={matches.length > 0 ? `cmdk-opt-${cursor}` : undefined}
            aria-label="Search commands"
            autoComplete="off"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setCursor(0);
            }}
            onKeyDown={handleKey}
            placeholder="Type a command…"
            className="w-full bg-transparent py-3 text-sm text-fg-default placeholder-fg-subtle outline-none"
          />
        </div>
        <div className="max-h-[50vh] overflow-auto">
          {matches.length === 0 ? (
            <div role="presentation" className="px-4 py-6 text-center text-xs text-fg-muted">
              No matching command.
            </div>
          ) : (
            <div id="cmdk-listbox" role="listbox" aria-label="Commands">
              {matches.map((a, i) => (
                <button
                  key={a.id}
                  type="button"
                  role="option"
                  id={`cmdk-opt-${i}`}
                  aria-selected={i === cursor}
                  tabIndex={-1}
                  onMouseMove={() => setCursor(i)}
                  onClick={() => {
                    onClose();
                    a.run();
                  }}
                  className={`w-full px-4 py-2 flex items-center gap-3 text-left text-sm ${
                    i === cursor ? "bg-accent-soft text-fg-default" : "text-fg-default hover:bg-surface-2"
                  }`}
                >
                  <span className="text-caption uppercase tracking-wide text-fg-subtle min-w-16 shrink-0">
                    {a.group}
                  </span>
                  <span className="flex-1 truncate">{a.title}</span>
                  {a.shortcut && <Kbd>{a.shortcut}</Kbd>}
                </button>
              ))}
            </div>
          )}
        </div>
        {/* Footer hint bar — the signature ⌘K affordance: keyboard legend so
            the palette reads as a navigable surface, not just a search box. */}
        <div className="flex items-center gap-3 border-t border-border-default px-4 py-1.5 text-caption text-fg-subtle">
          {PALETTE_HINTS.map((hint) => (
            <span key={hint.label} className="inline-flex items-center gap-1">
              {hint.keys.map((k) => (
                <Kbd key={k} className="px-1 py-px">
                  {k}
                </Kbd>
              ))}
              {hint.label}
            </span>
          ))}
          {matches.length > 0 && (
            <span className="ml-auto font-mono">
              {matches.length} result{matches.length === 1 ? "" : "s"}
            </span>
          )}
        </div>
      </div>
    </div>
  );
}
