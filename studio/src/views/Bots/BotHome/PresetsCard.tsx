// Extracted from BotHome/index.tsx to keep that file focused.
// Presets card — the launch presets the workflow declares.

import type { BotEntryWithSchema } from "@/api/bots";
import { Card } from "@/components/ui";

import SectionTitle from "./SectionTitle";

export default function PresetsCard({ entry }: { entry: BotEntryWithSchema }) {
  const presets = entry.presets?.entries ?? [];
  return (
    <Card>
      <SectionTitle flush>Presets</SectionTitle>
      <ul className="mt-1 space-y-1.5">
        {presets.map((p) => {
          const valueCount = p.values?.length ?? 0;
          return (
          <li key={p.name} className="rounded-md border border-border-default bg-surface-2 px-2 py-1.5">
            <div className="flex items-baseline gap-1.5">
              <span className="text-xs font-medium text-fg-default">
                {p.display_name?.trim() || p.name}
              </span>
              {p.display_name?.trim() && (
                <span className="font-mono text-caption text-fg-subtle">{p.name}</span>
              )}
              <span className="ml-auto text-caption text-fg-subtle">
                {valueCount} value{valueCount === 1 ? "" : "s"}
              </span>
            </div>
            {p.description && <p className="mt-0.5 text-caption text-fg-muted">{p.description}</p>}
          </li>
          );
        })}
      </ul>
    </Card>
  );
}
