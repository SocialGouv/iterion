import { Link } from "wouter";
import {
  DoubleArrowLeftIcon,
  GearIcon,
} from "@radix-ui/react-icons";

import NavLinks from "./NavLinks";
import OrgSwitcher from "./OrgSwitcher";
import RepoSwitcher from "./RepoSwitcher";
import SidebarContext from "./SidebarContext";
import { BrandMark } from "@/components/ui/BrandMark";
import { BrandWordmark } from "@/components/ui/BrandWordmark";
import { useUIStore } from "@/store/ui";

function openSettings() {
  window.dispatchEvent(new CustomEvent("iterion:open-settings"));
}

const SIDEBAR_W_EXPANDED = "w-[220px]";
const SIDEBAR_W_COLLAPSED = "w-[56px]";

// Numeric mirrors of the two widths above, for FIXED surfaces that must
// clear the sidebar (the floating assistant's resize clamp, which grows
// leftward). Tailwind only sees the string literals, so the numbers live
// beside them — change them together.
export const SIDEBAR_EXPANDED_PX = 220;
export const SIDEBAR_COLLAPSED_PX = 56;

// Sidebar is the persistent vertical chrome on the left of the studio.
// It hosts the iterion logo, the project + ⌘K context block, primary
// nav, and the footer with backend status / theme / user team chip.
//
// Collapse state is persisted in localStorage via the UI store. When
// collapsed (56px) every row degrades to an icon-only square button
// with native tooltips that preserve the labels.
export default function Sidebar() {
  const collapsed = useUIStore((s) => s.sidebarCollapsed);
  const toggle = useUIStore((s) => s.toggleSidebarCollapsed);

  return (
    <aside
      className={`shrink-0 ${collapsed ? SIDEBAR_W_COLLAPSED : SIDEBAR_W_EXPANDED} h-full bg-surface-1 border-r border-border-default flex flex-col transition-[width] duration-150 ease-out overflow-hidden`}
      aria-label="Primary"
    >
      {/* Brand + fold control. Expanded: the logo links Home and the
          ‹‹ button collapses. Collapsed: the logo *is* the expand control
          — clicking it unfolds the rail, so fold (‹‹, top) and unfold
          (the logo) live in the same top-left spot. */}
      <div
        className={`shrink-0 h-12 flex items-center ${collapsed ? "justify-center px-1.5" : "gap-2 px-3"} border-b border-border-default`}
      >
        {collapsed ? (
          <button
            type="button"
            onClick={toggle}
            className="inline-flex items-center justify-center hover:opacity-80 transition-opacity"
            title="Expand sidebar"
            aria-label="Expand sidebar"
            aria-expanded={false}
          >
            <BrandMark className="h-7 w-7 shrink-0" />
          </button>
        ) : (
          <>
            <Link
              href="/"
              className="flex items-center gap-2 min-w-0 hover:opacity-80 transition-opacity"
              title="Iterion home"
              aria-label="Iterion home"
            >
              <BrandMark className="h-7 w-7 shrink-0" />
              <BrandWordmark />
            </Link>
            <button
              type="button"
              onClick={toggle}
              className="ml-auto inline-flex items-center justify-center h-6 w-6 rounded text-fg-subtle hover:text-fg-default hover:bg-surface-2 transition-colors"
              title="Collapse sidebar"
              aria-label="Collapse sidebar"
              aria-expanded={true}
            >
              <DoubleArrowLeftIcon className="w-3.5 h-3.5" />
            </button>
          </>
        )}
      </div>

      {/* Dedicated organization switcher — the single, prominent place to
          switch org / open org settings / (super-admin) create one. Keeps the
          org context out of the bottom account chip. */}
      <div className={`shrink-0 ${collapsed ? "px-1.5 pt-2" : "px-2 pt-2"}`}>
        <OrgSwitcher collapsed={collapsed} />
      </div>

      {/* Repo-first context: the active repository scopes most views and
          pre-fills repo-targeting actions. Cloud-only; the zero-repo state
          doubles as the connect CTA. */}
      <div className={`shrink-0 ${collapsed ? "px-1.5 pt-1.5" : "px-2 pt-1.5"}`}>
        <RepoSwitcher collapsed={collapsed} />
      </div>

      {/* Context block: project + search/command palette */}
      <div className={`shrink-0 ${collapsed ? "px-1.5 py-2" : "px-2 py-2"}`}>
        <SidebarContext collapsed={collapsed} />
      </div>

      {/* Primary nav */}
      <div className={`flex-1 min-h-0 overflow-y-auto ${collapsed ? "px-1.5" : "px-2"}`}>
        <NavLinks collapsed={collapsed} />
      </div>

      {/* Footer: single Settings entry + user/team chip. Backend
          credentials and theme moved into the Settings dialog so the
          two sit together in a coherent panel instead of competing as
          mismatched chrome at the bottom of the sidebar. */}
      <div
        className={`shrink-0 border-t border-border-default ${collapsed ? "px-1.5 py-2 flex flex-col items-center gap-1.5" : "px-2 py-2 flex flex-col gap-1.5"}`}
      >
        {collapsed ? (
          <button
            type="button"
            onClick={openSettings}
            className="inline-flex items-center justify-center h-7 w-7 rounded text-fg-subtle hover:text-fg-default hover:bg-surface-2 transition-colors"
            title="Preferences"
            aria-label="Open preferences"
          >
            <GearIcon className="w-4 h-4" />
          </button>
        ) : (
          <button
            type="button"
            onClick={openSettings}
            className="flex w-full items-center gap-2 px-2 py-1 text-xs rounded text-fg-default hover:bg-surface-2 transition-colors"
            aria-label="Open settings"
          >
            <GearIcon className="w-4 h-4 shrink-0" />
            <span>Preferences</span>
          </button>
        )}
      </div>
    </aside>
  );
}
