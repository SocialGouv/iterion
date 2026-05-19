import { Link } from "wouter";
import type { ReactNode } from "react";

import NavLinks, { type Section } from "./NavLinks";
import ProjectLabel from "./ProjectLabel";
import UserTeamChip from "./UserTeamChip";
import BackendStatusPill from "@/components/Toolbar/BackendStatusPill";
import ThemeToggle from "@/components/ui/ThemeToggle";
import { useUIStore } from "@/store/ui";

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
          className="flex items-center hover:opacity-80 transition-opacity shrink-0"
          title="Iterion home"
          aria-label="Iterion home"
        >
          <img
            src="/favicon-96x96.png"
            alt="Iterion"
            className="h-7 w-7 dark:invert"
          />
        </Link>
        <NavLinks active={active} />
        <ProjectLabel />
        <CommandPaletteHint />
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

// CommandPaletteHint renders a small "⌘K" chip next to the project
// label so the global Cmd+K palette is discoverable. Click toggles
// the palette via the UI store; the actual shortcut is handled by
// GlobalCommandPalette. Hidden on narrow viewports to keep the
// header from wrapping.
function CommandPaletteHint() {
  const toggle = useUIStore((s) => s.toggleCommandPalette);
  const isMac =
    typeof navigator !== "undefined" &&
    navigator.userAgent.toLowerCase().includes("mac");
  const label = isMac ? "⌘K" : "Ctrl+K";
  return (
    <button
      type="button"
      onClick={toggle}
      className="hidden md:inline-flex items-center gap-1 px-1.5 py-0.5 text-[10px] rounded border border-border-default bg-surface-2 text-fg-subtle hover:text-fg-default hover:border-border-strong"
      title="Open command palette"
    >
      <kbd className="font-mono">{label}</kbd>
    </button>
  );
}
