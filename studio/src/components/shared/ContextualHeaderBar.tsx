import { isLocalIdentity, useAuth } from "@/auth/AuthContext";
import { useUIStore } from "@/store/ui";
import UserTeamChip from "./UserTeamChip";

// Slim header bar above <main>: the account menu pinned top-right (the
// standard spot) plus a page's optional left/right content (pushed via
// useHeaderSlot). In local dev the account chip self-hides (UserTeamChip
// renders null for the synthetic "dev" user), so when no page content is
// pushed either, the bar would be an empty grey strip — render nothing
// instead.
export default function ContextualHeaderBar() {
  const left = useUIStore((s) => s.headerLeft);
  const right = useUIStore((s) => s.headerRight);
  const { user } = useAuth();

  const chipHidden = isLocalIdentity(user);
  if (chipHidden && !left && !right) return null;

  return (
    <div className="shrink-0 h-10 flex items-center gap-3 px-3 sm:px-4 text-sm bg-surface-1 border-b border-border-default overflow-hidden">
      <div className="flex items-center gap-2 min-w-0 flex-1">{left}</div>
      <div className="flex items-center gap-1.5 sm:gap-2 shrink-0">
        {right}
        <UserTeamChip collapsed side="bottom" align="end" />
      </div>
    </div>
  );
}
