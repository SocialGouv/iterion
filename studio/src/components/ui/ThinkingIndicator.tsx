import { useEffect, useState } from "react";

// Shared loading-state primitive: a small inline ticker that cycles
// through `words`, typing each one out character-by-character before
// rotating to the next, prefixed by a pulsing ✻ glyph. Used by the
// Runs/logs ThinkingFooter, the WhatsNext NodeBanner per-node loader,
// and the WhatsNext PreFlightPanel — so the loading aesthetic stays
// identical across the studio (typing animation + glyph + mono italic
// styling), while each call site supplies its own word pool.

const ROTATE_MS = 2400;
const TYPE_MS = 35;

export interface ThinkingIndicatorProps {
  words: readonly string[];
  active: boolean;
  // Override the default styling. The default reproduces the original
  // ThinkingFooter look (mono italic, 11px, text-info-fg, fade-in).
  className?: string;
}

export function ThinkingIndicator({
  words,
  active,
  className = "font-mono text-[11px] text-info-fg italic px-1 py-0.5 animate-fade-in-opacity",
}: ThinkingIndicatorProps) {
  // Random initial index so two indicators mounted at the same instant
  // (parallel branches, or the preflight ticker handing off to a banner
  // ticker) don't lock-step through the same word sequence.
  const [idx, setIdx] = useState(() =>
    words.length > 0 ? Math.floor(Math.random() * words.length) : 0,
  );
  const [charCount, setCharCount] = useState(0);

  useEffect(() => {
    if (!active || words.length <= 1) return;
    const id = window.setInterval(() => {
      setIdx((i) => (i + 1) % words.length);
    }, ROTATE_MS);
    return () => window.clearInterval(id);
  }, [active, words.length]);

  // When the word list itself changes (e.g. PreFlightPanel transitioning
  // from "launching" to "first-event" phrases), snap the index back into
  // range instead of pointing at a stale slot.
  useEffect(() => {
    setIdx((i) => (words.length === 0 ? 0 : i % words.length));
  }, [words]);

  useEffect(() => {
    if (!active) return;
    setCharCount(0);
    const word = words[idx] ?? "";
    const id = window.setInterval(() => {
      setCharCount((n) => {
        if (n >= word.length) {
          window.clearInterval(id);
          return n;
        }
        return n + 1;
      });
    }, TYPE_MS);
    return () => window.clearInterval(id);
  }, [idx, active, words]);

  if (!active || words.length === 0) return null;

  const word = words[idx] ?? "";
  const shown = word.slice(0, charCount);
  const done = charCount >= word.length;

  return (
    <div aria-hidden="true" className={className}>
      <span className="mr-1 inline-block animate-pulse">✻</span>
      {shown}
      {done ? "…" : ""}
    </div>
  );
}
