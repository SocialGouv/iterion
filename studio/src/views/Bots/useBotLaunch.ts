// How THIS session launches a registry bot, for every affordance that offers
// it: the one decision (botLaunch), and — for the affordances that open the
// launch form — whether that form can open for the caller
// (botLaunchFormRefusal). A hook so that a Launch button, a Launch link and a
// Test pane read the same answer from the same facts, wherever they sit.

import type { OrgTreeView } from "@/api/auth";
import type { BotEntryWithSchema } from "@/api/bots";
import { useMaybeAuth } from "@/auth/AuthContext";
import { findTeamGrant, managesTeam } from "@/hooks/useTenantSubject";
import { useServerInfoStore } from "@/store/serverInfo";

import { botLaunch, botLaunchFormRefusal, botLaunchHref, type BotLaunch } from "./botPaths";

export interface BotLaunchWay {
  /** How the bot is launched, null when it cannot be (botLaunch). What a
   *  launch BY ID — the Test pane — needs, and all it needs. */
  launch: BotLaunch | null;
  /** The launch form's URL, null when the bot cannot be launched or its form
   *  cannot open for this caller. */
  formHref: string | null;
  /** Why the form cannot open although the bot can be launched, for the
   *  affordance's title; null otherwise. */
  formRefusal: string | null;
}

/**
 * The server's canEditBots, as far as the session can say it and no further:
 * a platform admin, an admin or owner of the team's org (managesTeam), or a
 * membership of the team that is admin, owner or config-editor. The server
 * reads that membership row itself; the session holds two pictures of it —
 * the role its credential carries for the active team, and the identity
 * tree's — and either can lag the row (a role granted since the credential
 * was issued, an org document that did not load). Either one saying so is
 * enough: the studio's reading only decides whether the form is OFFERED, and
 * the server answers the form.
 */
function editsTeamBots(
  auth: { orgs: OrgTreeView[]; activeTeamID: string; activeRole: string | null },
  isSuperAdmin: boolean,
): boolean {
  if (managesTeam(auth.orgs, isSuperAdmin, auth.activeTeamID)) return true;
  const treeRole = findTeamGrant(auth.orgs, auth.activeTeamID)?.team.role ?? null;
  return [auth.activeRole, treeRole].some(
    (role) => role === "config_editor" || role === "admin" || role === "owner",
  );
}

export function useBotLaunch(entry: BotEntryWithSchema | null | undefined): BotLaunchWay {
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  // No session to read (a component mounted alone) is a caller the studio
  // knows nothing of: nothing is refused here, and the server answers.
  const auth = useMaybeAuth();
  if (!entry) return { launch: null, formHref: null, formRefusal: null };
  const launch = botLaunch(entry, cloud);
  const isSuperAdmin = !!auth?.user?.is_super_admin;
  const formRefusal =
    launch && auth
      ? botLaunchFormRefusal(entry, cloud, {
          editsTeamBots: editsTeamBots(auth, isSuperAdmin),
        })
      : null;
  return {
    launch,
    formHref: launch && !formRefusal ? botLaunchHref(launch) : null,
    formRefusal,
  };
}
