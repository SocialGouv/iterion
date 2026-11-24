// Extracted from LaunchView.tsx to keep that file focused.
// useBotPresets resolves the registry bot behind the launch — the bot it
// names, or the bundle whose main.bot the open file is — and owns preset
// selection: applying a named preset overlays its values onto the current
// var form state.

import { useEffect, useMemo, useState } from "react";
import type { Dispatch, SetStateAction } from "react";

import type { IterDocument, Preset } from "@/api/types";
import { useBotsStore } from "@/store/bots";

import { literalToString, pickPresets } from "./utils";

export function useBotPresets(
  filePath: string,
  doc: IterDocument | null,
  setValues: Dispatch<SetStateAction<Record<string, string>>>,
  /** Set on a launch that NAMES its bot (a cloud launch by slug): its
   *  canonical id, and the presets the server read with its program.
   *
   *  A named bot is its registry entry whatever tier serves it — a stored bot
   *  (a team's own, a platform override) has no workspace path to be matched
   *  by — but the entry supplies only what it alone knows and what no launch
   *  runs: the bot's identity, its launch hints and its repository
   *  requirement. The PRESETS are the pinned read's: the registry listing is
   *  as old as the session, and a preset's values are applied by the server
   *  from the program — a stale list would show values the run does not
   *  get. */
  named: { botID: string; presets: Preset[] } | null = null,
  /** The landing the form shows (useLaunchDoc's `load`): made anew each time
   *  a load resets the form's values, whatever document it carries. */
  load: object | null = doc,
) {
  // Prefer the bot schema's presets (the union of in-source `presets:` and
  // file-based presets/<name>.md, carrying display_name / description / prompt
  // / skills) when the open file is a bundle's main.bot; fall back to the
  // workflow doc's in-source presets for a loose .bot file. A named launch
  // takes neither: its presets came with its program.
  const allBots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    if (allBots === null) void fetchBots();
  }, [allBots, fetchBots]);
  const botID = named?.botID ?? "";
  const bot = useMemo(
    () =>
      (botID
        ? allBots?.find((b) => b.name === botID)
        : allBots?.find(
            (b) => b.is_bundle && b.rel_path && filePath === `${b.rel_path}/main.bot`,
          )) ?? null,
    [allBots, filePath, botID],
  );
  const presets = named ? named.presets : (bot?.presets?.entries ?? pickPresets(doc));
  const [selectedPreset, setSelectedPreset] = useState<string>("");
  // The selection belongs to the LANDING it was made on. A reload, or a move
  // to another target, lands again and resets the form's values to the
  // declared defaults (useLaunchDoc's `land`): a selection that outlived it
  // would post a preset whose values the form no longer holds, shadowed by an
  // explicit default for every key it covers. Keyed on the landing and not on
  // the document, which a later load can hand back as the very same object —
  // the values would be reset and the selection kept.
  const [selectedOn, setSelectedOn] = useState(load);
  if (selectedOn !== load) {
    setSelectedOn(load);
    setSelectedPreset("");
  }
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
