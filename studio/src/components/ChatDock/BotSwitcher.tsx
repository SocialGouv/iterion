// Which conversational bot answers in the dock.
//
// Renders nothing when the registry has one entry: a picker with a single
// option is chrome that teaches the operator nothing. It appears the moment a
// second bot declares a `chat:` block — which is the whole point of making
// the registry manifest-driven, and the only studio-side change a new chat
// bot ever gets.

import { Select } from "@/components/ui/Select";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";

export default function BotSwitcher({
  bots,
  current,
  onSelect,
}: {
  bots: readonly FirstClassBot[];
  current: FirstClassBot;
  onSelect: (id: string) => void;
}) {
  if (bots.length < 2) return null;
  return (
    <label className="flex items-center min-w-0">
      <span className="sr-only">Assistant bot</span>
      <Select
        fit
        size="sm"
        value={current.id}
        onChange={(e) => onSelect(e.target.value)}
        // The description is the tooltip: the difference between a co-CTO for
        // your repo and an assistant that knows iterion itself is not
        // recoverable from a persona name.
        title={current.description || current.label}
        className="max-w-[11rem]"
      >
        {bots.map((b) => (
          <option key={b.id} value={b.id}>
            {b.label}
          </option>
        ))}
      </Select>
    </label>
  );
}
