import { useMemo } from "react";

import type { BotEntry } from "@/api/bots";
import { Combobox, type ComboboxOption } from "@/components/ui/Combobox";

interface Props {
  value: string;
  bots: BotEntry[];
  onChange: (botName: string) => void;
  disabled?: boolean;
  /** Pre-baked tags shown to the right of each row (default: triggers
   *  + capabilities, comma-joined and truncated). */
  metaFor?: (bot: BotEntry) => string;
}

const defaultMeta = (b: BotEntry): string => {
  const tags = [...(b.triggers ?? []), ...(b.capabilities ?? [])];
  if (tags.length === 0) return "";
  const joined = tags.join(", ");
  return joined.length > 32 ? joined.slice(0, 29) + "…" : joined;
};

/** BotPicker is the Board ticket form's bot selector. Empty value =
 *  "use dispatcher default" (the per-assignee / global workflow from
 *  the dispatcher config). A non-empty value overrides that choice at
 *  launch time (pkg/dispatcher/loop.go buildSpec).
 */
export function BotPicker({
  value,
  bots,
  onChange,
  disabled,
  metaFor = defaultMeta,
}: Props) {
  const options = useMemo<ComboboxOption<string>[]>(
    () =>
      bots.map((b) => ({
        value: b.name,
        label: b.name,
        description: b.description,
        meta: metaFor(b) || undefined,
        searchHaystack: `${b.name} ${b.description ?? ""} ${(b.triggers ?? []).join(" ")} ${(b.capabilities ?? []).join(" ")}`,
      })),
    [bots, metaFor],
  );

  return (
    <Combobox<string>
      value={value}
      options={options}
      onChange={(v) => onChange(v)}
      emptyLabel="(dispatcher default)"
      emptyDescription="Use the workflow configured on the dispatcher."
      placeholder="Search bots…"
      disabled={disabled}
    />
  );
}
