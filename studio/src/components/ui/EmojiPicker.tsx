import { useState, type KeyboardEvent, type ReactNode } from "react";

import { Input } from "./Input";
import { Popover } from "./Popover";

/** Curated grid — a bots/tools/animals/objects mix that covers the
 *  personalities catalog bots tend to want. Free-text below the grid
 *  accepts any emoji not listed here. */
const CURATED_EMOJI = [
  // bots & tech
  "🤖", "🧭", "🛠️", "🔧", "⚙️", "🔩", "🧪", "🔬",
  "🛰️", "📡", "💻", "🖥️", "⌨️", "🧮", "🔌", "💾",
  // work & inspection
  "🔎", "🔍", "🛡️", "🔒", "🔑", "📦", "🚀", "⚡",
  "🧹", "🪄", "🧰", "📐", "✂️", "🪛", "🧲", "🗜️",
  // docs & planning
  "📚", "📖", "📝", "🗒️", "📋", "🗂️", "🗺️", "📊",
  "📈", "🎯", "🏁", "⏱️", "📅", "💡", "🧠", "💬",
  // animals & nature
  "🦉", "🦊", "🐙", "🐝", "🐢", "🦅", "🐺", "🦫",
  "🌿", "🌍", "🌱", "🍀", "🔥", "🌊", "⭐", "🎨",
];

export interface EmojiPickerProps {
  /** The element that opens the picker (rendered as the popover trigger). */
  trigger: ReactNode;
  /** Called with the chosen emoji (grid click, or free-text submit). */
  onSelect: (emoji: string) => void;
  side?: "top" | "right" | "bottom" | "left";
  align?: "start" | "center" | "end";
}

/**
 * EmojiPicker is a small self-contained popover: a curated emoji grid
 * plus a free-text input that accepts any emoji (paste or OS emoji
 * keyboard). No emoji-data dependency — the grid is a hand-picked set
 * and the free-text path covers everything else. Selecting closes the
 * popover.
 */
export function EmojiPicker({ trigger, onSelect, side = "bottom", align = "start" }: EmojiPickerProps) {
  const [open, setOpen] = useState(false);
  const [custom, setCustom] = useState("");

  const pick = (emoji: string) => {
    onSelect(emoji);
    setCustom("");
    setOpen(false);
  };

  const submitCustom = () => {
    const v = custom.trim();
    if (v) pick(v);
  };

  const onCustomKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter") {
      e.preventDefault();
      submitCustom();
    }
  };

  return (
    <Popover
      trigger={trigger}
      open={open}
      onOpenChange={setOpen}
      side={side}
      align={align}
      contentClassName="w-64 p-2"
    >
      <div className="grid grid-cols-8 gap-0.5" role="listbox" aria-label="Pick an emoji">
        {CURATED_EMOJI.map((e) => (
          <button
            key={e}
            type="button"
            role="option"
            aria-selected={false}
            aria-label={`Select ${e}`}
            className="flex h-7 w-7 items-center justify-center rounded text-base leading-none hover:bg-surface-2 focus:bg-surface-2 focus:outline-none"
            onClick={() => pick(e)}
          >
            {e}
          </button>
        ))}
      </div>
      <div className="mt-2 flex gap-1 border-t border-border-default pt-2">
        <Input
          type="text"
          value={custom}
          onChange={(e) => setCustom(e.target.value)}
          onKeyDown={onCustomKeyDown}
          placeholder="Any emoji…"
          aria-label="Custom emoji"
          className="min-w-0 flex-1"
        />
        <button
          type="button"
          className="shrink-0 rounded bg-surface-2 px-2 text-xs text-fg-default hover:bg-surface-3 disabled:cursor-not-allowed disabled:opacity-40"
          onClick={submitCustom}
          disabled={!custom.trim()}
          aria-label="Use custom emoji"
        >
          Use
        </button>
      </div>
    </Popover>
  );
}
