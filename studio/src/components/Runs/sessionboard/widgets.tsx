import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

import type { SessionBoardWidget } from "@/api/runs/types";

// Renderers for the LLM-curated Session-board widgets. Each widget is a
// card composed from the studio's design tokens; chart widgets use
// Recharts with token-fed colors so they inherit the theme. Unknown kinds
// render nothing (forward-compat). Built in-house — no dashboard framework
// — so the board feels native and stays themeable.

// Default export so SessionBoardTab can React.lazy() this module — that
// keeps Recharts (a heavy d3-derived dependency) out of the main RunView
// chunk and loads it only when a run actually has curated widgets.
export default function SessionWidgets({
  widgets,
}: {
  widgets: SessionBoardWidget[];
}) {
  if (widgets.length === 0) return null;
  return (
    <section className="flex flex-col gap-2">
      <h3 className="text-caption font-semibold uppercase tracking-wide text-fg-subtle">
        Session
      </h3>
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
        {widgets.map((w) => (
          <WidgetCard key={w.id} widget={w} />
        ))}
      </div>
    </section>
  );
}

function WidgetCard({ widget }: { widget: SessionBoardWidget }) {
  const body = renderBody(widget);
  if (body === null) return null;
  return (
    <div className="rounded-lg border border-border-default bg-surface-1 p-3 flex flex-col gap-2">
      {widget.title && (
        <div className="text-label font-semibold text-fg-default">
          {widget.title}
        </div>
      )}
      {body}
    </div>
  );
}

function renderBody(w: SessionBoardWidget): React.ReactNode {
  const p = w.props ?? {};
  switch (w.kind) {
    case "note":
      return <NoteBody text={str(p.text)} />;
    case "metric":
      return <MetricBody value={p.value} hint={str(p.hint)} />;
    case "checklist":
      return <ChecklistBody items={p.items} />;
    case "progress":
      return <ProgressBody value={num(p.value)} max={num(p.max)} />;
    case "bar_chart":
      return <BarChartBody data={p.data} />;
    default:
      return null; // unknown kind — forward-compat
  }
}

function NoteBody({ text }: { text: string }) {
  if (!text) return null;
  return <p className="text-body text-fg-muted leading-snug">{text}</p>;
}

function MetricBody({ value, hint }: { value: unknown; hint: string }) {
  return (
    <div className="flex items-baseline gap-2">
      <span className="text-display font-semibold text-fg-default tabular-nums">
        {scalar(value)}
      </span>
      {hint && <span className="text-caption text-fg-subtle">{hint}</span>}
    </div>
  );
}

function ChecklistBody({ items }: { items: unknown }) {
  const list = Array.isArray(items) ? items : [];
  if (list.length === 0) return null;
  return (
    <ul className="flex flex-col gap-0.5">
      {list.map((raw, idx) => {
        const item = (raw ?? {}) as Record<string, unknown>;
        const done = item.done === true;
        return (
          <li key={idx} className="flex items-start gap-1.5 leading-snug">
            <span
              className={`flex-none mt-px ${done ? "text-success-fg" : "text-fg-subtle"}`}
              aria-hidden
            >
              {done ? "●" : "○"}
            </span>
            <span className={done ? "text-fg-subtle line-through" : "text-fg-default"}>
              {str(item.text)}
            </span>
          </li>
        );
      })}
    </ul>
  );
}

function ProgressBody({ value, max }: { value: number; max: number }) {
  const total = max > 0 ? max : 0;
  const pct = total > 0 ? Math.min(100, Math.round((value / total) * 100)) : 0;
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-baseline justify-between text-caption text-fg-subtle">
        <span className="tabular-nums">
          {value}
          {total > 0 ? ` / ${total}` : ""}
        </span>
        <span className="tabular-nums">{pct}%</span>
      </div>
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface-3">
        <div
          className="h-full rounded-full bg-accent transition-all"
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}

interface BarDatum {
  label: string;
  value: number;
}

function BarChartBody({ data }: { data: unknown }) {
  const rows: BarDatum[] = Array.isArray(data)
    ? data
        .map((raw) => {
          const d = (raw ?? {}) as Record<string, unknown>;
          return { label: str(d.label), value: num(d.value) };
        })
        .filter((d) => d.label !== "")
    : [];
  if (rows.length === 0) return null;
  return (
    <div className="h-40 w-full">
      <ResponsiveContainer width="100%" height="100%">
        <BarChart data={rows} margin={{ top: 4, right: 4, bottom: 4, left: 4 }}>
          <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border-subtle)" />
          <XAxis
            dataKey="label"
            tick={{ fill: "var(--color-fg-subtle)", fontSize: 10 }}
            stroke="var(--color-border-default)"
          />
          <YAxis
            tick={{ fill: "var(--color-fg-subtle)", fontSize: 10 }}
            stroke="var(--color-border-default)"
            allowDecimals={false}
          />
          <Tooltip
            cursor={{ fill: "var(--color-surface-2)" }}
            contentStyle={{
              background: "var(--color-surface-2)",
              border: "1px solid var(--color-border-default)",
              borderRadius: 6,
              fontSize: 11,
            }}
          />
          <Bar dataKey="value" fill="var(--color-accent)" radius={[3, 3, 0, 0]} />
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

// --- defensive coercion of the free-form props payload ---

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function num(v: unknown): number {
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string") {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}

function scalar(v: unknown): string {
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return "—";
}
