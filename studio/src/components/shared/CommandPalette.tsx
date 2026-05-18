import { useEffect, useMemo, useRef, useState } from "react";

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

  useEffect(() => {
    if (open) {
      setQuery("");
      setCursor(0);
      // setTimeout puts the focus call after the dialog mount so the
      // input receives the focus reliably across React 19 + Radix.
      const id = setTimeout(() => inputRef.current?.focus(), 10);
      return () => clearTimeout(id);
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
      className="fixed inset-0 z-[var(--z-confirm)] bg-black/40 flex items-start justify-center pt-[15vh]"
      onClick={onClose}
    >
      <div
        className="w-[min(560px,calc(100vw-2rem))] rounded-lg border border-border-default bg-surface-1 shadow-2xl overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        <input
          ref={inputRef}
          type="text"
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            setCursor(0);
          }}
          onKeyDown={handleKey}
          placeholder="Type a command…"
          className="w-full bg-transparent px-4 py-3 text-sm text-fg-default placeholder-fg-subtle outline-none border-b border-border-default"
        />
        <div className="max-h-[50vh] overflow-auto">
          {matches.length === 0 && (
            <div className="px-4 py-6 text-center text-xs text-fg-muted">
              No matching command.
            </div>
          )}
          {matches.map((a, i) => (
            <button
              key={a.id}
              type="button"
              onMouseMove={() => setCursor(i)}
              onClick={() => {
                onClose();
                a.run();
              }}
              className={`w-full px-4 py-2 flex items-center gap-3 text-left text-sm ${
                i === cursor ? "bg-accent-soft text-fg-default" : "text-fg-default hover:bg-surface-2"
              }`}
            >
              <span className="text-[10px] uppercase tracking-wide text-fg-subtle min-w-[64px] shrink-0">
                {a.group}
              </span>
              <span className="flex-1 truncate">{a.title}</span>
              {a.shortcut && (
                <kbd className="font-mono text-[10px] px-1.5 py-0.5 rounded bg-surface-2 border border-border-default text-fg-muted">
                  {a.shortcut}
                </kbd>
              )}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
