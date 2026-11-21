// A "Launch" affordance for a registry bot, wherever a view lists bots: it
// leads to the bot's launch form the way this session launches that bot
// (useBotLaunch) — by name in cloud, by its workflow file locally. A bot that
// cannot be launched renders nothing; a bot whose form cannot open for this
// caller renders the affordance held, saying why and how the bot can still be
// launched — reachable by keyboard as well as by mouse (HeldControl). A list
// renders one per row, so its accessible name carries the bot it launches,
// the visible word first.

import { useLocation } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
import HeldControl from "@/components/shared/HeldControl";
import { Button } from "@/components/ui";

import { useBotLaunch } from "./useBotLaunch";

export default function BotLaunchLink({
  entry,
  label = "Launch",
  size,
}: {
  entry: BotEntryWithSchema | null | undefined;
  label?: string;
  size?: "sm" | "md";
}) {
  const [, setLocation] = useLocation();
  const { launch, formHref, formRefusal } = useBotLaunch(entry);
  if (!entry || !launch) return null;
  return (
    <HeldControl reason={formRefusal}>
      <Button
        variant="secondary"
        size={size}
        aria-label={`${label} ${entry.display_name || entry.name}`}
        onClick={() => {
          if (formHref) setLocation(formHref);
        }}
      >
        {label}
      </Button>
    </HeldControl>
  );
}
