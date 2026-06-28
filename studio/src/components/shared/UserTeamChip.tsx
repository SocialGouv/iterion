import { useState } from "react";
import { CaretSortIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import { hasOrgRole, useAuth } from "@/auth/AuthContext";
import { Popover, PopoverClose } from "@/components/ui/Popover";

// Hidden entirely in local dev mode (user id "dev"), where the desktop
// app's native menus drive Settings / ProjectSwitcher instead.
export default function UserTeamChip({ collapsed = false }: { collapsed?: boolean }) {
  const {
    user,
    orgs,
    teams,
    activeOrg,
    activeOrgID,
    activeOrgRole,
    activeTeamID,
    activeTeam,
    signOut,
    selectOrg,
    selectTeam,
  } = useAuth();
  const [, navigate] = useLocation();
  const [open, setOpen] = useState(false);

  const isLocal = user?.id === "dev";
  if (isLocal) return null;

  const orgLabel = activeOrg?.org_name ?? "No organization";
  const teamLabel = activeTeam?.team_name ?? "No team";
  const canManageActiveOrg =
    !!user?.is_super_admin || hasOrgRole(activeOrgRole, "admin");
  // Avatar initials from the ORG name (fallback to the user's email).
  const initials =
    orgLabel
      .split(/\s+/)
      .filter(Boolean)
      .slice(0, 2)
      .map((w) => w[0])
      .join("")
      .toUpperCase() || (user?.email?.[0]?.toUpperCase() ?? "?");

  // PopoverClose wraps each menu button so the popover dismisses on
  // click — equivalent to the previous setOpen(false) lines but keyboard-
  // accessible (Radix wires Escape + focus return for us).
  const closeAfter = (fn: () => void) => () => {
    fn();
    setOpen(false);
  };

  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      side="top"
      align="start"
      contentClassName="w-[min(20rem,calc(100vw-1rem))] p-2 text-sm"
      trigger={
        collapsed ? (
          <button
            type="button"
            className="inline-flex h-7 w-7 items-center justify-center rounded-full bg-accent text-fg-onAccent text-caption font-semibold uppercase hover:opacity-80 transition-opacity"
            title={`${orgLabel} / ${teamLabel} · ${user?.email ?? ""}`}
            aria-label={`Account menu — ${orgLabel} / ${teamLabel}, ${user?.email ?? ""}`}
          >
            {initials}
          </button>
        ) : (
          <button
            type="button"
            className="flex w-full items-center gap-2 rounded px-2 py-1 text-left hover:bg-surface-2 transition-colors"
            title={`${orgLabel} / ${teamLabel} · ${user?.email ?? ""}`}
            aria-label={`Account menu — ${orgLabel} / ${teamLabel}, ${user?.email ?? ""}`}
          >
            <span className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-accent text-fg-onAccent text-caption font-semibold uppercase">
              {initials}
            </span>
            <span className="min-w-0 flex-1 leading-tight">
              <span className="block truncate text-xs font-medium text-fg-default">
                <span className="text-fg-muted">{orgLabel}</span>
                {activeTeam && (
                  <>
                    <span className="text-fg-subtle"> / </span>
                    {teamLabel}
                  </>
                )}
              </span>
              {user?.email && (
                <span className="block truncate text-caption text-fg-muted">
                  {user.email}
                </span>
              )}
            </span>
            <CaretSortIcon className="h-4 w-4 shrink-0 text-fg-subtle" />
          </button>
        )
      }
    >
      {orgs.length > 1 && (
        <>
          <div className="px-2 py-1 text-xs uppercase tracking-wider text-fg-muted">
            Switch organization
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
          <div className="my-1 border-t border-border-subtle" />
        </>
      )}
      {teams.length > 1 && (
        <>
          <div className="px-2 py-1 text-xs uppercase tracking-wider text-fg-muted">
            Switch team
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
          <div className="my-1 border-t border-border-subtle" />
        </>
      )}
      {teams.length === 0 && activeOrg && (
        <div className="px-2 py-1.5 text-xs text-fg-muted">
          No teams in this organization
          {canManageActiveOrg && (
            <>
              {" — "}
              <PopoverClose asChild>
                <button
                  onClick={closeAfter(() => navigate(`/orgs/${activeOrgID}?tab=teams`))}
                  className="text-accent hover:underline"
                >
                  create one
                </button>
              </PopoverClose>
            </>
          )}
        </div>
      )}
      {activeOrg && canManageActiveOrg && (
        <PopoverClose asChild>
          <button
            onClick={closeAfter(() => navigate(`/orgs/${activeOrgID}`))}
            className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2"
          >
            Manage {activeOrg.org_name}
          </button>
        </PopoverClose>
      )}
      {activeTeam && (
        <PopoverClose asChild>
          <button
            onClick={closeAfter(() => navigate(`/teams/${activeTeam.team_id}`))}
            className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2"
          >
            Manage {activeTeam.team_name}
          </button>
        </PopoverClose>
      )}
      <PopoverClose asChild>
        <button
          onClick={closeAfter(() => navigate("/account"))}
          className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2"
        >
          Account settings
        </button>
      </PopoverClose>
      {user?.is_super_admin && (
        <>
          <PopoverClose asChild>
            <button
              onClick={closeAfter(() => navigate("/admin/orgs"))}
              className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 text-warning-fg"
            >
              Platform admin · Organizations
            </button>
          </PopoverClose>
          <PopoverClose asChild>
            <button
              onClick={closeAfter(() => navigate("/admin/users"))}
              className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 text-warning-fg"
            >
              Platform admin · Users
            </button>
          </PopoverClose>
        </>
      )}
      <PopoverClose asChild>
        <button
          onClick={closeAfter(() => void signOut())}
          className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 text-danger"
        >
          Sign out
        </button>
      </PopoverClose>
    </Popover>
  );
}
