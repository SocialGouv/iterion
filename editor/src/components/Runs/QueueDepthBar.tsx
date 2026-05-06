import type { RunStatus } from "@/api/runs";

// QueueDepthBar shows a compact aggregate of run states across the
// store: queued ⧗  running ▶  paused ⏸. It sits at the top of the
// runs list as a single line so an operator can read backlog,
// in-flight, and stuck-on-human work at a glance without scanning
// the table. Hidden when no runs are in any of the tracked buckets.
//
// Cloud-ready plan §F (T-15).

interface Props {
  counts: Partial<Record<RunStatus, number>>;
}

interface Cell {
  status: RunStatus;
  glyph: string;
  label: string;
  // Tailwind text colour token; chosen to match the row badge tone
  // without re-using <Badge/> here (the bar is denser than a row).
  textClass: string;
}

const CELLS: Cell[] = [
  { status: "queued", glyph: "⧗", label: "queued", textClass: "text-fg-muted" },
  { status: "running", glyph: "▶", label: "running", textClass: "text-info" },
  {
    status: "paused_waiting_human",
    glyph: "⏸",
    label: "paused",
    textClass: "text-warning",
  },
];

export default function QueueDepthBar({ counts }: Props) {
  const total = CELLS.reduce((acc, c) => acc + (counts[c.status] ?? 0), 0);
  if (total === 0) return null;

  return (
    <div className="px-4 py-1.5 flex items-center gap-4 border-b border-border-default bg-surface-1 text-[11px]">
      <span className="text-fg-subtle uppercase tracking-wide">In flight</span>
      {CELLS.map((cell) => {
        const n = counts[cell.status] ?? 0;
        return (
          <span
            key={cell.status}
            className={`inline-flex items-center gap-1 ${
              n > 0 ? cell.textClass : "text-fg-subtle/50"
            }`}
            title={`${n} ${cell.label}`}
          >
            <span aria-hidden>{cell.glyph}</span>
            <span className="font-mono">{n}</span>
            <span className="text-fg-subtle">{cell.label}</span>
          </span>
        );
      })}
    </div>
  );
}
