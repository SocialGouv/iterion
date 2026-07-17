import { useState } from "react";
import { CaretSortIcon, PlusIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import { hasOrgRole, useAuth } from "@/auth/AuthContext";
import { Popover, PopoverClose } from "@/components/ui/Popover";

// Dedicated organization switcher, pinned at the top of the sidebar
// (GitHub/Linear pattern). It owns ALL org-level affordances — switch org,
// open org settings, and (super-admin) jump to the org console to create one —
// so the bottom account chip (UserTeamChip) no longer carries a flat org list.
//
// Hidden in local dev (user id "dev") and for restricted/no-org users, where
// there is no org context to switch.
function orgInitials(name: string, fallback: string): string {
  return (
    name
      .split(/\s+/)
      .filter(Boolean)
      .slice(0, 2)
      .map((w) => w[0])
      .join("")
      .toUpperCase() || fallback
  );
}

export default function OrgSwitcher({ collapsed = false }: { collapsed?: boolean }) {
  const {
    user,
    orgs,
    activeOrg,
    activeOrgID,
    activeOrgRole,
    activeTeamID,
    teams,
    selectOrg,
    selectTeam,
  } = useAuth();
  const [, navigate] = useLocation();
  const [open, setOpen] = useState(false);

  const isLocal = user?.id === "dev";
  // No org context (restricted/no-org user) → nothing to switch.
  if (isLocal || orgs.length === 0) return null;

  const orgLabel = activeOrg?.org_name ?? "Select organization";
  // Every resource is team-scoped, so the active team is context the
  // operator should always be able to see — even in the common
  // single-team org where no switching is possible.
  const activeTeamName =
    teams.find((t) => t.team_id === activeTeamID)?.team_name ?? null;
  const canManageActiveOrg = !!user?.is_super_admin || hasOrgRole(activeOrgRole, "admin");
  const isSuper = user?.is_super_admin ?? false;
  const initials = orgInitials(orgLabel, user?.email?.[0]?.toUpperCase() ?? "?");

  const closeAfter = (fn: () => void) => () => {
    fn();
    setOpen(false);
  };

  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      side="bottom"
      align="start"
      contentClassName="w-[min(20rem,calc(100vw-1rem))] p-2 text-sm"
      trigger={
        collapsed ? (
          // Full-width h-8 row to match the collapsed NavRow footprint (which is
          // `h-8 w-full`), with the org avatar centered — otherwise the org icon
          // reads as narrower than the nav icons below it.
          <button
            type="button"
            className="flex h-8 w-full items-center justify-center rounded hover:bg-surface-2 transition-colors"
            title={`Organization: ${orgLabel}`}
            aria-label={`Switch organization — current: ${orgLabel}`}
          >
            <span className="inline-flex h-5 w-5 items-center justify-center rounded-md bg-accent text-fg-onAccent text-caption font-semibold uppercase">
              {initials}
            </span>
          </button>
        ) : (
          <button
            type="button"
            className="flex w-full items-center gap-2 rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-left hover:bg-surface-2 transition-colors"
            title={`Organization: ${orgLabel}`}
            aria-label={`Switch organization — current: ${orgLabel}`}
          >
            <span className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-md bg-accent text-fg-onAccent text-caption font-semibold uppercase">
              {initials}
            </span>
            <span className="min-w-0 flex-1 leading-tight">
              <span className="block text-caption text-fg-muted">Organization</span>
              <span className="block truncate text-xs font-medium text-fg-default">
                {orgLabel}
              </span>
              {activeTeamName && activeTeamName !== orgLabel && (
                <span className="block truncate text-caption text-fg-subtle">
                  team: {activeTeamName}
                </span>
              )}
            </span>
            <CaretSortIcon className="h-4 w-4 shrink-0 text-fg-subtle" />
          </button>
        )
      }
    >
      <div className="px-2 py-1 text-xs uppercase tracking-wider text-fg-muted">
        Organizations
      </div>
      {orgs.map((o) => (
        <PopoverClose asChild key={o.org_id}>
          <button
            onClick={closeAfter(() => void selectOrg(o.org_id))}
            className={`w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 ${
              o.org_id === activeOrgID ? "bg-surface-2" : ""
            }`}
          >
            <div className="font-medium">{o.org_name}</div>
            <div className="text-xs text-fg-muted">
              {o.org_role || "member"}
              {o.personal && " · personal"}
            </div>
          </button>
        </PopoverClose>
      ))}
      {/* Teams stay escamotées: the studio reads Org → Repo. The team —
          the resource tenant — only surfaces here when the org actually
          has several, as a section of the org menu (never a separate
          switcher in the chrome). */}
      {teams.length > 1 && (
        <>
          <div className="my-1 border-t border-border-subtle" />
          <div className="px-2 py-1 text-xs uppercase tracking-wider text-fg-muted">
            Teams
          </div>
          {teams.map((t) => (
            <PopoverClose asChild key={t.team_id}>
              <button
                onClick={closeAfter(() => void selectTeam(t.team_id))}
                className={`w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 ${
                  t.team_id === activeTeamID ? "bg-surface-2" : ""
                }`}
              >
                <div className="font-medium">{t.team_name}</div>
                <div className="text-xs text-fg-muted">
                  {t.role}
                  {t.personal && " · personal"}
                </div>
              </button>
            </PopoverClose>
          ))}
        </>
      )}
      {teams.length === 0 && activeOrg && canManageActiveOrg && (
        <>
          <div className="my-1 border-t border-border-subtle" />
          <div className="px-2 py-1.5 text-xs text-fg-muted">
            No teams in this organization —{" "}
            <PopoverClose asChild>
              <button
                onClick={closeAfter(() => navigate(`/orgs/${activeOrgID}?tab=teams`))}
                className="text-accent hover:underline"
              >
                create one
              </button>
            </PopoverClose>
          </div>
        </>
      )}
      <div className="my-1 border-t border-border-subtle" />
      {/* Every member can open the org page (Plan, Usage, Teams roster are
          member-readable; the page hides mutating controls itself) — only
          the label signals the admin's extra powers. */}
      {activeOrg && (
        <PopoverClose asChild>
          <button
            onClick={closeAfter(() => navigate(`/orgs/${activeOrgID}`))}
            className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2"
          >
            {canManageActiveOrg ? "Organization settings" : "Organization"}
          </button>
        </PopoverClose>
      )}
      {isSuper && (
        <PopoverClose asChild>
          <button
            onClick={closeAfter(() => navigate("/admin/orgs"))}
            className="flex w-full items-center gap-1.5 px-2 py-1.5 rounded hover:bg-surface-2 text-warning-fg"
          >
            <PlusIcon className="h-4 w-4 shrink-0" />
            New organization…
          </button>
        </PopoverClose>
      )}
    </Popover>
  );
}
