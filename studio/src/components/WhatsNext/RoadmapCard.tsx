import type { RoadmapCardMessage, RoadmapItem } from "@/lib/whats-next/messages";
import { Badge } from "@/components/ui";

interface Props {
  message: RoadmapCardMessage;
}

export default function RoadmapCard({ message }: Props) {
  const { roadmap, iteration } = message;

  return (
    <div className="rounded-lg border border-border-default bg-surface-2 p-3 space-y-3">
      <div className="flex items-baseline justify-between gap-2">
        <h3 className="text-[13px] font-semibold text-fg-default">
          {iteration === 0 ? "Proposed roadmap" : `Revised roadmap (iter ${iteration})`}
        </h3>
        <span className="text-[10px] text-fg-subtle font-mono">{message.nodeId}</span>
      </div>

      {roadmap.rationale && (
        <p className="text-[12px] text-fg-muted whitespace-pre-wrap break-words border-l-2 border-border-subtle pl-2">
          {roadmap.rationale}
        </p>
      )}

      {roadmap.next_action && (
        <Section title="Next action" tone="accent">
          <ItemRow item={roadmap.next_action} />
        </Section>
      )}

      {roadmap.short_term.length > 0 && (
        <Section title="Short term" tone="default">
          {roadmap.short_term.map((it, i) => (
            <ItemRow key={i} item={it} />
          ))}
        </Section>
      )}

      {roadmap.long_term.length > 0 && (
        <Section title="Long term" tone="muted">
          {roadmap.long_term.map((it, i) => (
            <ItemRow key={i} item={it} />
          ))}
        </Section>
      )}
    </div>
  );
}

function Section({
  title,
  tone,
  children,
}: {
  title: string;
  tone: "accent" | "default" | "muted";
  children: React.ReactNode;
}) {
  const toneClass =
    tone === "accent"
      ? "text-accent"
      : tone === "muted"
        ? "text-fg-muted"
        : "text-fg-default";
  return (
    <div className="space-y-1.5">
      <div
        className={`text-[10px] uppercase tracking-wide font-medium ${toneClass}`}
      >
        {title}
      </div>
      <div className="space-y-1.5">{children}</div>
    </div>
  );
}

function ItemRow({ item }: { item: RoadmapItem }) {
  return (
    <div className="rounded border border-border-subtle bg-surface-1 p-2 space-y-1">
      <div className="flex items-baseline gap-2">
        <span className="text-[12px] font-medium text-fg-default">
          {item.title}
        </span>
        {item.assignee && (
          <Badge variant="neutral" size="sm">
            {item.assignee}
          </Badge>
        )}
      </div>
      {item.body && (
        <p className="text-[11px] text-fg-muted whitespace-pre-wrap break-words">
          {item.body}
        </p>
      )}
    </div>
  );
}
