import { isLocalIdentity, useAuth } from "@/auth/AuthContext";
import { useUIStore } from "@/store/ui";
import UserTeamChip from "./UserTeamChip";

// Slim header bar above <main>: the account menu pinned top-right (the
// standard spot) plus a page's optional left/right content (pushed via
// useHeaderSlot). It is the page's BANNER landmark (RGAA 9.2): exactly one
// per page, always rendered — in local dev the account chip self-hides
// (UserTeamChip renders null for the synthetic "dev" user) and the bar
// stays as the empty strip rather than dropping the landmark.
export default function ContextualHeaderBar() {
  const left = useUIStore((s) => s.headerLeft);
  const right = useUIStore((s) => s.headerRight);
  const { user } = useAuth();

  const chipHidden = isLocalIdentity(user);
  const empty = chipHidden && !left && !right;

  return (
    <header
      className={`shrink-0 flex items-center gap-3 px-3 sm:px-4 text-sm bg-surface-1 border-b border-border-default overflow-hidden ${empty ? "h-0 border-b-0" : "h-10"}`}
    >
      <div className="flex items-center gap-2 min-w-0 flex-1">{left}</div>
      <div className="flex items-center gap-1.5 sm:gap-2 shrink-0">
        {right}
        <UserTeamChip collapsed side="bottom" align="end" />
      </div>
    </header>
  );
}
