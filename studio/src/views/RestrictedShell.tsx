import { lazy, Suspense } from "react";
import { useLocation } from "wouter";

import { Button } from "@/components/ui/Button";
import { BrandMark } from "@/components/ui/BrandMark";
import { BrandWordmark } from "@/components/ui/BrandWordmark";
import { ThemeToggle } from "@/components/ui/ThemeToggle";
import { InlineBanner } from "@/components/ui/InlineBanner";
import MainSpinner from "@/components/shared/MainSpinner";
import { useAuth } from "@/auth/AuthContext";

const MarketplaceView = lazy(() => import("@/views/Marketplace"));

/**
 * RestrictedShell is the marketplace-only surface shown to the "submitter"
 * tier — a signed-in user who belongs to no authorized team (the public
 * GitHub SSO sign-up that matched no allow-listed team). They can browse,
 * download, and submit bots to the marketplace, but have no workspace, so
 * the full studio (editor, runs, board, launch) is intentionally absent.
 * An admin adding them to a team upgrades them on next login.
 */
export default function RestrictedShell() {
  const { user, signOut } = useAuth();
  const [, navigate] = useLocation();
  return (
    <div className="flex h-screen min-h-0 flex-col bg-surface-0 text-fg-default">
      <header className="flex items-center justify-between border-b border-border-subtle px-4 py-3 sm:px-6">
        <div className="flex items-center gap-2.5">
          <BrandMark className="h-7 w-7" />
          <BrandWordmark />
        </div>
        <div className="flex items-center gap-2 sm:gap-3">
          <ThemeToggle />
          <span className="hidden text-xs text-fg-muted sm:inline">{user?.email}</span>
          <Button variant="secondary" size="sm" onClick={() => void signOut()}>
            Sign out
          </Button>
        </div>
      </header>

      <div className="border-b border-border-subtle px-4 py-3 sm:px-6">
        <InlineBanner
          tone="info"
          layout="inline"
          action={
            <Button
              variant="secondary"
              size="sm"
              onClick={() => navigate("/invitations/accept")}
            >
              I have an invitation
            </Button>
          }
        >
          You're signed in, but not a member of any team yet. You can browse and
          submit bots to the marketplace below — ask an administrator to add you
          to a team for full access. Received an invite link or token? Redeem it
          here.
        </InlineBanner>
      </div>

      <div className="min-h-0 flex-1 overflow-hidden">
        <Suspense fallback={<MainSpinner />}>
          <MarketplaceView />
        </Suspense>
      </div>
    </div>
  );
}
