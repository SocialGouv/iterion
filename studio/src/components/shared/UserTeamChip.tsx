import { useState } from "react";
import { useLocation } from "wouter";

import { useAuth } from "@/auth/AuthContext";
import { Popover, PopoverClose } from "@/components/ui/Popover";

// Hidden entirely in local dev mode (user id "dev"), where the desktop
// app's native menus drive Settings / ProjectSwitcher instead.
export default function UserTeamChip() {
  const { user, teams, activeTeamID, activeTeam, signOut, selectTeam } = useAuth();
  const [, navigate] = useLocation();
  const [open, setOpen] = useState(false);

  const isLocal = user?.id === "dev";
  if (isLocal) return null;

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
      side="bottom"
      align="end"
      contentClassName="w-[min(18rem,calc(100vw-1rem))] p-2 text-sm"
      trigger={
        <button
          className="bg-surface-1/95 border border-border-subtle rounded px-3 py-1 text-xs flex items-center gap-2 shadow max-w-[160px] sm:max-w-none"
          title={`${activeTeam?.team_name ?? "No team"} · ${user?.email ?? ""}`}
        >
          <span className="font-medium truncate">{activeTeam?.team_name ?? "No team"}</span>
          {/* Hide email on narrow viewports — team name is enough chrome. */}
          <span className="hidden sm:inline text-fg-muted truncate">{user?.email}</span>
          <span aria-hidden="true">▾</span>
        </button>
      }
    >
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
        <PopoverClose asChild>
          <button
            onClick={closeAfter(() => navigate("/admin"))}
            className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 text-fg-warn"
          >
            Platform admin
          </button>
        </PopoverClose>
      )}
      <PopoverClose asChild>
        <button
          onClick={closeAfter(() => void signOut())}
          className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 text-fg-error"
        >
          Sign out
        </button>
      </PopoverClose>
    </Popover>
  );
}
