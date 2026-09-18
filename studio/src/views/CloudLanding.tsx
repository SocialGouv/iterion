import { Suspense, lazy, useEffect } from "react";
import { useLocation } from "wouter";
import { Button } from "@/components/ui/Button";
import { BrandWordmark } from "@/components/ui/BrandWordmark";
import { BrandMark } from "@/components/ui/BrandMark";
import { ThemeToggle } from "@/components/ui/ThemeToggle";
import BootLoading from "@/components/shared/BootLoading";
import { useServerInfoStore } from "@/store/serverInfo";
import { SignInCard } from "./Login";

// This module is one of App.tsx's few EAGER view imports, because
// PublicTopBar (below) renders on /marketplace outside the lazy route tree.
// So the product page — its own stylesheet plus ~50 icon modules — is
// lazy()'d here rather than statically imported: an operator who goes straight
// to a studio URL never opens it, and a static import would put it in the
// entry chunk every one of them downloads on first paint.
const CloudHome = lazy(() => import("./CloudHome"));

// PublicTopBar is the slim header shown above the public Marketplace view
// when an anonymous visitor browses it (that route lives outside the
// authenticated AppShell). Brand links home (the landing); a theme toggle +
// Sign-in button sit on the right.
export function PublicTopBar() {
  const [, navigate] = useLocation();
  return (
    <div className="sticky top-0 z-10 flex items-center justify-between border-b border-border-subtle bg-surface-0/90 px-4 py-3 backdrop-blur sm:px-6">
      <button
        type="button"
        onClick={() => navigate("/")}
        className="flex items-center gap-2.5 rounded text-fg-default hover:opacity-80 focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
        aria-label="Back to home"
      >
        <BrandMark className="h-7 w-7" />
        <BrandWordmark />
      </button>
      <div className="flex items-center gap-2 sm:gap-3">
        <ThemeToggle />
        <Button variant="primary" size="sm" onClick={() => navigate("/login")}>
          Sign in
        </Button>
      </div>
    </div>
  );
}

// The product page is cloud-only, and it is what "/" serves — to an anonymous
// visitor and to a signed-in operator alike, which is why the studio lives
// under its own base. Deployments that do not run in cloud mode have no
// product page: an anonymous visitor gets the sign-in card here, and a
// signed-in one never reaches this component (AuthGate sends the root straight
// into the studio).
//
// signedIn comes from the caller rather than from useAuth(): this is the
// product page, and a page has no business requiring the auth provider to be
// mounted above it just to label a button.
export default function CloudLanding({ signedIn = false }: { signedIn?: boolean }) {
  const serverInfo = useServerInfoStore((s) => s.info);
  useEffect(() => {
    if (!serverInfo) void useServerInfoStore.getState().refresh();
  }, [serverInfo]);
  if (serverInfo?.mode !== "cloud") {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
        <SignInCard />
      </div>
    );
  }
  return (
    <Suspense fallback={<BootLoading />}>
      <CloudHome
        marketplaceEnabled={serverInfo.marketplace_enabled}
        signedIn={signedIn}
      />
    </Suspense>
  );
}
