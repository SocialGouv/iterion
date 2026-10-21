import { useEffect, useState } from "react";

const SPINNER_FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
const SPINNER_MS = 80;
const ELAPSED_MS = 250;

// ToolRunningFooter mirrors ThinkingFooter's slot in the Logs panel's
// virtuoso Footer. It renders while a tool is in flight, swapped in
// place of the random-words loader so the user can see exactly what
// the run is waiting on (with elapsed time) instead of a generic
// "thinking" indicator. The component re-renders on its own interval
// so the elapsed counter ticks without the store having to publish a
// timer event.
export function ToolRunningFooter({
  toolName,
  startedAt,
}: {
  toolName: string;
  startedAt: number;
}) {
  const [frame, setFrame] = useState(0);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = window.setInterval(() => {
      setFrame((n) => (n + 1) % SPINNER_FRAMES.length);
    }, SPINNER_MS);
    return () => window.clearInterval(id);
  }, []);

  useEffect(() => {
    const id = window.setInterval(() => {
      setNow(Date.now());
    }, ELAPSED_MS);
    return () => window.clearInterval(id);
  }, []);

  const elapsed = formatElapsed(Math.max(0, now - startedAt));
  const label = toolName || "tool";

  return (
    <div
      aria-live="polite"
      className="font-mono text-micro text-info-fg italic px-1 py-0.5 animate-fade-in-opacity flex items-center gap-2"
    >
      <span className="text-accent-text">{SPINNER_FRAMES[frame]}</span>
      <span>Running {label}</span>
      <span className="ml-auto text-fg-subtle not-italic">{elapsed}</span>
    </div>
  );
}

function formatElapsed(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.floor(s % 60);
  return `${m}m${rem.toString().padStart(2, "0")}s`;
}
