import { useMemo, useState } from "react";

import type { RunEvent } from "@/api/runs";
import { Input } from "@/components/ui";
import { useToggleSet } from "@/hooks/useToggleSet";

export function EventsTab({ events }: { events: RunEvent[] }) {
  const [search, setSearch] = useState("");
  const activeTypesSet = useToggleSet<string>();
  const activeTypes = activeTypesSet.set;
  const [showRawData, setShowRawData] = useState(false);

  // Counts per type so the chip row can show occurrence numbers.
  const typeCounts = useMemo(() => {
    const m = new Map<string, number>();
    for (const e of events) m.set(e.type, (m.get(e.type) ?? 0) + 1);
    return m;
  }, [events]);

  const knownTypes = useMemo(
    () => Array.from(typeCounts.keys()).sort(),
    [typeCounts],
  );

  const filtered = useMemo(() => {
    const query = search.trim().toLowerCase();
    return events.filter((e) => {
      if (activeTypes.size > 0 && !activeTypes.has(e.type)) return false;
      if (!query) return true;
      // Search type, node_id, and stringified data so users can grep
      // for a substring (e.g. "rate_limit" or "tool_name=Bash").
      if (e.type.toLowerCase().includes(query)) return true;
      if (e.node_id?.toLowerCase().includes(query)) return true;
      if (e.data && JSON.stringify(e.data).toLowerCase().includes(query))
        return true;
      return false;
    });
  }, [events, activeTypes, search]);

  const toggleType = activeTypesSet.toggle;

  return (
    <div className="h-full flex flex-col">
      <div className="px-4 py-2 border-b border-border-default space-y-1.5">
        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search events…"
          size="sm"
          leadingIcon={<span className="text-caption">⌕</span>}
        />
        {knownTypes.length > 1 && (
          <div className="flex flex-wrap gap-1">
            {knownTypes.map((t) => {
              const isActive = activeTypes.has(t);
              return (
                <button
                  key={t}
                  type="button"
                  onClick={() => toggleType(t)}
                  aria-pressed={isActive}
                  className={`text-caption px-1.5 py-0.5 rounded border transition-colors ${
                    isActive
                      ? "bg-accent-soft border-accent text-fg-default"
                      : "bg-surface-1 border-border-default text-fg-subtle hover:text-fg-default"
                  }`}
                >
                  {t} <span className="text-fg-subtle">{typeCounts.get(t)}</span>
                </button>
              );
            })}
          </div>
        )}
        <div className="flex items-center gap-2">
          <span className="text-caption text-fg-subtle">
            {filtered.length} / {events.length} events
          </span>
          <button
            type="button"
            onClick={() => setShowRawData((v) => !v)}
            aria-pressed={showRawData}
            className="ml-auto text-caption text-fg-subtle hover:text-fg-default"
          >
            {showRawData ? "hide raw" : "show raw"}
          </button>
        </div>
      </div>
      <div className="flex-1 overflow-auto px-4 py-2">
        {filtered.length === 0 ? (
          <div className="text-fg-subtle">No events match.</div>
        ) : (
          <ul className="space-y-0.5 font-mono text-caption">
            {filtered.map((e) => (
              <li key={`${e.run_id}:${e.seq}`}>
                <div className="flex gap-2">
                  <span className="text-fg-subtle">
                    {e.seq.toString().padStart(4, "0")}
                  </span>
                  <span>{e.type}</span>
                </div>
                {showRawData && e.data && Object.keys(e.data).length > 0 && (
                  <pre className="ml-12 my-0.5 text-fg-subtle whitespace-pre-wrap break-all">
                    {JSON.stringify(e.data, null, 2)}
                  </pre>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
