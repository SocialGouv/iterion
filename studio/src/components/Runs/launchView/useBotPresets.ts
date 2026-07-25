// Extracted from LaunchView.tsx to keep that file focused.
// useBotPresets resolves the bot bundle behind the open file (when the
// file is a bundle's main.bot) and owns preset selection: applying a
// named preset overlays its values onto the current var form state.

import { useEffect, useMemo, useState } from "react";
import type { Dispatch, SetStateAction } from "react";

import type { IterDocument } from "@/api/types";
import { useBotsStore } from "@/store/bots";

import { literalToString, pickPresets } from "./utils";

export function useBotPresets(
  filePath: string,
  doc: IterDocument | null,
  setValues: Dispatch<SetStateAction<Record<string, string>>>,
) {
  // Prefer the bot schema's presets (the union of in-source `presets:` and
  // file-based presets/<name>.md, carrying display_name / description / prompt
  // / skills) when the open file is a bundle's main.bot; fall back to the
  // workflow doc's in-source presets for a loose .bot file.
  const allBots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    if (allBots === null) void fetchBots();
  }, [allBots, fetchBots]);
  const bot = useMemo(
    () =>
      allBots?.find(
        (b) => b.is_bundle && b.rel_path && filePath === `${b.rel_path}/main.bot`,
      ) ?? null,
    [allBots, filePath],
  );
  const presets = bot?.presets?.entries ?? pickPresets(doc);
  const [selectedPreset, setSelectedPreset] = useState<string>("");
  const selectedPresetMeta = useMemo(
    () => presets.find((p) => p.name === selectedPreset),
    [presets, selectedPreset],
  );

  // Apply a named preset by overlaying its values onto the current form
  // state. Existing values for keys not in the preset are preserved, so
  // switching from "prod" to "dev" updates only the overlapping keys —
  // which is the same precedence as the engine.
  const applyPreset = (name: string) => {
    setSelectedPreset(name);
    if (!name) return;
    const preset = presets.find((p) => p.name === name);
    if (!preset) return;
    setValues((prev) => {
      const next = { ...prev };
      for (const pv of preset.values) {
        next[pv.key] = literalToString(pv.value);
      }
      return next;
    });
  };

  return { bot, presets, selectedPreset, selectedPresetMeta, applyPreset };
}
