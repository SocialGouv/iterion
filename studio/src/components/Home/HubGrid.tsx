import { useQuery } from "@tanstack/react-query";
import { Link } from "wouter";
import {
  CubeIcon,
  ListBulletIcon,
  ViewGridIcon,
  CardStackIcon,
  RocketIcon,
  LightningBoltIcon,
  ArchiveIcon,
  Component1Icon,
  MixIcon,
  LockClosedIcon,
  Link2Icon,
  GearIcon,
  ReaderIcon,
} from "@radix-ui/react-icons";

import { useAuth, canEditConfigShares } from "@/auth/AuthContext";
import { useServerInfoStore } from "@/store/serverInfo";
import { listConfigShares, type ShareView } from "@/api/configShareAdmin";

/**
 * HubGrid — the "everything you can reach" panel on the Home landing.
 *
 * The Sidebar's NavLinks is the source of truth for which surfaces a user
 * can access (server_info.*_enabled flags + role/team/super-admin checks);
 * this grid mirrors that SAME gating so the root page exposes every area the
 * operator has access to, with a one-line description each — a hub, not a
 * dead end. Areas the server hasn't wired simply don't render.
 *
 * The Veilles (config-share) card is the exception the nav never had: there
 * is no top-level config-shares route, only the per-bot manager. The card
 * loads the team's shares and links straight to the owning bot's home (its
 * real `bot_id`, e.g. feed-watch) — never a stale default — so the veille is
 * represented correctly here instead of pointing at the wrong bot.
 */

interface AreaCard {
  href: string;
  label: string;
  desc: string;
  icon: typeof CubeIcon;
}

export default function HubGrid() {
  const info = useServerInfoStore((s) => s.info);
  const { activeTeam, activeTeamID, activeRole, user } = useAuth();
  const cloud = info?.mode === "cloud";
  const canEditVeilles = canEditConfigShares(activeRole, !!user?.is_super_admin);

  // Config-shares (Veilles) — loaded only when the feature is wired and a
  // team is active. Shares the ["config-shares", teamID] cache with the
  // per-bot ConfigSharesCard. Best-effort: a failure just hides the card
  // (the nav never surfaced it either, so its absence is not a regression).
  const sharesEnabled = !!info?.config_shares_enabled && !!activeTeamID && canEditVeilles;
  const sharesQuery = useQuery<ShareView[]>({
    queryKey: ["config-shares", activeTeamID],
    queryFn: () => listConfigShares(activeTeamID),
    enabled: sharesEnabled,
  });
  const shares = sharesEnabled ? (sharesQuery.data ?? null) : null;

  const areas: AreaCard[] = [
    { href: "/bots", label: "Bots", desc: "Browse and launch the bot catalog", icon: CubeIcon },
    { href: "/runs", label: "Runs", desc: "Every run, live and past", icon: ListBulletIcon },
  ];
  if (info?.native_tracker_enabled) {
    areas.push({ href: "/board", label: "Board", desc: "Kanban of issues and work", icon: ViewGridIcon });
    areas.push({ href: "/pipelines", label: "Pipelines", desc: "Concurrent root pipelines", icon: CardStackIcon });
  }
  if (info?.dispatcher_enabled) {
    areas.push({ href: "/dispatcher", label: "Dispatcher", desc: "Tracker → run, live", icon: RocketIcon });
  }
  if (info?.triggers_enabled || cloud) {
    areas.push({ href: "/triggers", label: "Automations", desc: "Schedules, webhooks, triggers", icon: LightningBoltIcon });
  }
  if (info?.marketplace_enabled) {
    areas.push({ href: "/marketplace", label: "Marketplace", desc: "Install bots and plugins", icon: ArchiveIcon });
  }
  if (info?.plugins_enabled) {
    areas.push({ href: "/plugins", label: "Plugins", desc: "Rewriters, MCP, skills", icon: Component1Icon });
  }
  if (info?.skills_enabled) {
    areas.push({ href: "/skills", label: "Skills", desc: "Your local skill library", icon: MixIcon });
  }
  if (info?.secrets_enabled) {
    areas.push({ href: "/secrets", label: "Secrets", desc: "API keys and named secrets", icon: LockClosedIcon });
  }
  if (activeTeam && cloud) {
    areas.push({ href: "/integrations", label: "Integrations", desc: "Connect repos and forges", icon: Link2Icon });
  }
  if (user?.is_super_admin && cloud) {
    areas.push({ href: "/admin/orgs", label: "Admin", desc: "Orgs, audit, platform", icon: GearIcon });
  }

  // The Veilles card leads to the dedicated /veilles editor route (the same
  // Workspace as the minimal config-editor shell). Shown with the share count
  // so the operator knows the scope.
  const veilleCount = shares && shares.length > 0 ? shares.length : 0;

  return (
    <section aria-label="Explore" className="space-y-2">
      <h2 className="px-0.5 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
        Explore
      </h2>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
        {veilleCount > 0 && (
          <HubTile
            href="/veilles"
            label="Veilles"
            desc={`${veilleCount} config-share${veilleCount === 1 ? "" : "s"} to edit`}
            icon={ReaderIcon}
          />
        )}
        {areas.map((a) => (
          <HubTile key={a.href} href={a.href} label={a.label} desc={a.desc} icon={a.icon} />
        ))}
      </div>
    </section>
  );
}

function HubTile({ href, label, desc, icon: Icon }: AreaCard) {
  return (
    <Link
      href={href}
      className="group flex flex-col gap-1.5 rounded-md border border-border-subtle bg-surface-1 p-3 transition-colors hover:border-border-default hover:bg-surface-2 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
    >
      <span className="inline-flex items-center gap-2">
        <Icon className="h-4 w-4 shrink-0 text-accent-text" />
        <span className="text-sm font-medium text-fg-default">{label}</span>
      </span>
      <span className="text-caption leading-snug text-fg-muted">{desc}</span>
    </Link>
  );
}
