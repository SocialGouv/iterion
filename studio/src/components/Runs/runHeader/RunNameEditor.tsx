// Extracted from RunHeader.tsx to keep that file focused.
// Inline edit affordance for the run name (Enter commits, Esc cancels).

import { useEffect, useRef, useState } from "react";

// RunNameEditor is the inline edit affordance for the run name. Mounted
// only while editing; auto-focuses and selects on mount. Enter commits,
// Escape (or blur) cancels. The id stays stable — the rename only
// updates the human-readable label.
export default function RunNameEditor({
  initial,
  onSubmit,
  onCancel,
}: {
  initial: string;
  onSubmit: (next: string) => void;
  onCancel: () => void;
}) {
  const [value, setValue] = useState(initial);
  const ref = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.focus();
    el.select();
  }, []);
  return (
    <input
      ref={ref}
      type="text"
      value={value}
      onChange={(e) => setValue(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          onSubmit(value);
        } else if (e.key === "Escape") {
          e.preventDefault();
          onCancel();
        }
      }}
      onBlur={() => onSubmit(value)}
      className="font-medium text-sm bg-surface-2 border border-border-control rounded px-1.5 py-0.5 min-w-[16rem] max-w-md focus:outline-none focus:border-accent"
      maxLength={200}
      aria-label="Rename run"
    />
  );
}
