import { useRef, useState, type KeyboardEvent } from "react";

interface Props {
  value: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  disabled?: boolean;
  /** Optional max length per tag (chars). Tags exceeding this length
   *  are silently truncated on commit. Useful for `labels:` which we
   *  want to keep readable. */
  maxTagLength?: number;
}

/** TagInput is a chip-based editor for string arrays. Used in the
 *  Board ticket form for labels, replacing the previous comma-separated
 *  text field. Commits a tag on Enter or comma; Backspace on an empty
 *  draft removes the last tag.
 *
 *  Tags are deduplicated case-insensitively but stored with the
 *  first-seen casing.
 */
export function TagInput({
  value,
  onChange,
  placeholder = "Add tag…",
  disabled = false,
  maxTagLength = 64,
}: Props) {
  const [draft, setDraft] = useState("");
  const inputRef = useRef<HTMLInputElement | null>(null);

  const commit = (raw: string) => {
    const trimmed = raw.trim().slice(0, maxTagLength);
    if (!trimmed) return;
    const exists = value.some((t) => t.toLowerCase() === trimmed.toLowerCase());
    if (exists) {
      setDraft("");
      return;
    }
    onChange([...value, trimmed]);
    setDraft("");
  };

  const remove = (idx: number) => {
    const next = value.slice();
    next.splice(idx, 1);
    onChange(next);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      commit(draft);
      return;
    }
    if (e.key === "Backspace" && draft === "" && value.length > 0) {
      e.preventDefault();
      remove(value.length - 1);
    }
  };

  return (
    <div
      className={`flex flex-wrap items-center gap-1 min-h-7 bg-surface-1 text-fg-default rounded-md border border-border-strong px-1.5 py-1 focus-within:border-accent focus-within:ring-1 focus-within:ring-accent ${disabled ? "opacity-60 cursor-not-allowed" : ""}`}
      onClick={() => inputRef.current?.focus()}
    >
      {value.map((tag, i) => (
        <span
          key={`${tag}-${i}`}
          className="inline-flex items-center gap-1 bg-surface-2 text-fg-default rounded-full px-2 py-0.5 text-[11px]"
        >
          {tag}
          <button
            type="button"
            aria-label={`Remove ${tag}`}
            onClick={(e) => {
              e.stopPropagation();
              if (!disabled) remove(i);
            }}
            className="text-fg-subtle hover:text-fg-default leading-none"
          >
            ×
          </button>
        </span>
      ))}
      <input
        ref={inputRef}
        type="text"
        value={draft}
        disabled={disabled}
        placeholder={value.length === 0 ? placeholder : ""}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={onKeyDown}
        onBlur={() => commit(draft)}
        className="flex-1 min-w-[6rem] bg-transparent text-xs outline-none placeholder:text-fg-subtle"
      />
    </div>
  );
}
