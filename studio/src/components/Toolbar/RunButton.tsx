import { useEffect } from "react";
import { ExclamationTriangleIcon, PlayIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import { parseBotSourceEditorPath } from "@/api/client";
import { useMaybeAuth } from "@/auth/AuthContext";
import HeldControl from "@/components/shared/HeldControl";
import { Button, IconButton } from "@/components/ui";
import { launchRefusal } from "@/lib/launch";
import { resolveOpenBot } from "@/lib/startSources";
import { useBackendDetectStore } from "@/store/backendDetect";
import { useBotsStore } from "@/store/bots";
import { useDocumentStore } from "@/store/document";
import { useServerInfoStore } from "@/store/serverInfo";
import { botLaunch, botLaunchHref, type BotLaunch } from "@/views/Bots/botPaths";

// The toolbar's Run: launches the bound file by path, or an unsaved buffer
// inline. Enabled exactly when the launch view would launch — it reads the
// same `launchRefusal` — so the button never leads to an empty picker or to
// a launch the server refuses: a fresh buffer, a salvage and a document with
// errors disable it, and its title says which.
//
// In cloud two things differ. The host credential probe says nothing there —
// a run is funded by the team's own keys, which the server resolves at
// launch — so it neither disables the button nor raises the nudge. And a
// file that is a bot's entry leads to that bot's NAMED launch form, so the
// run carries the bot's identity; any other file, and a buffer bound to
// none, launch as they do locally.
//
// Which bot a file is has two answers, and neither is "none" by default. A
// team's bot opened from its source names itself in its path
// (`botsource://<team>/<slug>/main.bot`), so it needs no catalog. Any other
// bound file is a registry bot's entry or not according to the catalog
// (resolveOpenBot) — and while the catalog is unknown, Run is held: launching
// the file by path meanwhile would run a bot's entry as unnamed work.
//
// Either way a bound file is launched as it is STORED: the launch form reads
// the file (or the bot), not this tab's buffer, so edits not yet saved are
// not what runs.
export default function RunButton() {
  const [, setLocation] = useLocation();
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const refusal = useDocumentStore((s) => launchRefusal(s));
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const hasResolvedBackend = useBackendDetectStore((s) => !!s.report?.resolved_default);
  // `report != null` once the host probe has returned (success or fail) —
  // gates the missing-credential nudge so it doesn't flash during the boot probe.
  const backendProbed = useBackendDetectStore((s) => s.report != null);
  const missingCredential = !cloud && !hasResolvedBackend;

  // The bot the open file is, in cloud only: its Run is the bot's named
  // launch. The catalog is asked for only where it decides something.
  const bots = useBotsStore((s) => s.bots);
  const botsError = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  const activeTeamID = useMaybeAuth()?.activeTeamID ?? "";
  const ownSource = cloud && currentFilePath ? parseBotSourceEditorPath(currentFilePath) : null;
  // The active team's own bot, by the path it was opened from.
  const ownBot =
    ownSource && ownSource.rel === "main.bot" && activeTeamID !== "" && ownSource.teamID === activeTeamID
      ? ownSource.slug
      : "";
  // A stored bot of ANOTHER team than the active one — a tab left open across
  // a team switch. Its launch by name belongs to that team, and launched from
  // here it would run as inline, unnamed work under this one.
  const otherTeam =
    ownSource && activeTeamID !== "" && ownSource.teamID !== activeTeamID
      ? `This tab is the bot ${ownSource.slug} of another team than the active one: switch to that team to launch it by name.`
      : null;
  // Every other bound file takes the catalog to say whether it is a bot.
  const catalogDecides = cloud && !!currentFilePath && !ownBot && !otherTeam;
  useEffect(() => {
    if (catalogDecides && bots === null) void fetchBots();
  }, [catalogDecides, bots, fetchBots]);
  const catalogBot = catalogDecides ? resolveOpenBot(bots, currentFilePath, activeTeamID) : undefined;
  const named: BotLaunch | null = ownBot
    ? { by: "bot", bot: ownBot }
    : catalogBot
      ? botLaunch(catalogBot, true)
      : null;
  const namedBot = named?.by === "bot" ? named.bot : "";
  const catalogUnknown = catalogDecides && bots === null
    ? botsError
      ? `The bot catalog did not load (${botsError}), so Run cannot tell whether this file is a bot's entry — reload the page.`
      : "Loading the bot catalog, to tell whether this file is a bot's entry…"
    : null;
  const held = otherTeam ?? catalogUnknown;

  return (
    <>
      {backendProbed && missingCredential && !refusal && (
        // The credential is the only thing holding this launch: surface a
        // clickable nudge straight to Preferences → Backends — otherwise the
        // only signal is a silently greyed-out button.
        <IconButton
          variant="warning"
          size="sm"
          label="No LLM credential detected — open Preferences → Backends"
          tooltip="No LLM credential detected — click to open Preferences → Backends"
          onClick={() =>
            window.dispatchEvent(
              new CustomEvent("iterion:open-settings", {
                detail: { tab: "backends" },
              }),
            )
          }
        >
          <ExclamationTriangleIcon />
        </IconButton>
      )}
      {/* What the named launch holds Run for — the catalog not known yet,
          another team's bot — is said where a keyboard reaches it
          (HeldControl); the editor's own refusals disable the button. */}
      <HeldControl reason={refusal || missingCredential ? null : held}>
        <Button
          variant="primary"
          size="sm"
          leadingIcon={<PlayIcon />}
          onClick={() => {
            if (held) return;
            setLocation(
              named
                ? botLaunchHref(named)
                : currentFilePath
                  ? `/runs/new?file=${encodeURIComponent(currentFilePath)}`
                  : `/runs/new`,
            );
          }}
          disabled={!!refusal || missingCredential}
          title={
            refusal ??
            (missingCredential
              ? "No LLM credentials detected — open Preferences → Backends to configure."
              : held
                ? undefined
                : namedBot
                  ? `Launch the bot ${namedBot}`
                  : currentFilePath
                    ? `Launch ${currentFilePath}`
                    : "Launch the unsaved workflow")
          }
        >
          Run
        </Button>
      </HeldControl>
    </>
  );
}
