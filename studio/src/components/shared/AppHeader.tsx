import { Link } from "wouter";
import type { ReactNode } from "react";

import NavLinks, { type Section } from "./NavLinks";
import ProjectLabel from "./ProjectLabel";
import UserTeamChip from "./UserTeamChip";
import BackendStatusPill from "@/components/Toolbar/BackendStatusPill";
import ThemeToggle from "@/components/ui/ThemeToggle";

interface AppHeaderProps {
  active?: Section;
  children?: ReactNode;
  rightActions?: ReactNode;
  showBackendPill?: boolean;
}

export default function AppHeader({
  active,
  children,
  rightActions,
  showBackendPill = true,
}: AppHeaderProps) {
  return (
    <>
      {/* Skip-link surfaces only on keyboard focus; pages that mark
       * their main work surface with id="main-content" will be
       * jumped to. Pages without the anchor degrade gracefully — the
       * link is still present but inert. */}
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-[var(--z-toast)] focus:bg-accent focus:text-fg-onAccent focus:px-3 focus:py-1.5 focus:rounded focus:shadow-lg"
      >
        Skip to main content
      </a>
      <header className="shrink-0 h-12 flex items-center gap-2 sm:gap-3 px-3 sm:px-4 text-sm bg-surface-1 border-b border-border-default overflow-hidden">
        <Link
          href="/"
          className="font-bold tracking-wide text-sm hover:text-accent transition-colors shrink-0"
          title="Iterion home"
        >
          ITERION
        </Link>
        <NavLinks active={active} />
        <ProjectLabel />
        {children}
        <div className="ml-auto flex items-center gap-1.5 sm:gap-2 shrink-0">
          {rightActions}
          {showBackendPill && <BackendStatusPill />}
          <ThemeToggle />
          <UserTeamChip />
        </div>
      </header>
    </>
  );
}
