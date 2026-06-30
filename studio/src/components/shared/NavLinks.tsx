import { useState } from "react";
import { Link, useLocation } from "wouter";
import {
  HomeIcon,
  Pencil2Icon,
  ListBulletIcon,
  ViewGridIcon,
  RocketIcon,
  LightningBoltIcon,
  PaperPlaneIcon,
  ArchiveIcon,
  ChevronDownIcon,
  ChevronRightIcon,
  Cross2Icon,
  PlayIcon,
  Link2Icon,
  GearIcon,
  Component1Icon,
} from "@radix-ui/react-icons";
import { useShallow } from "zustand/react/shallow";

import { useAuth } from "@/auth/AuthContext";
import { useServerInfoStore } from "@/store/serverInfo";
import {
  selectEditorTabs,
  selectRunTabs,
  useTabsStore,
  type Tab,
} from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";

export type Section =
  | "home"
  | "whatsNext"
  | "editor"
  | "runs"
  | "board"
  | "dispatcher"
  | "triggers"
  | "marketplace"
  | "plugins"
  | "org"
  | "team"
  | "integrations"
  | "admin";

interface Props {
  collapsed: boolean;
}

interface LinkDef {
  section: Section;
  href: string;
  label: string;
  icon: typeof HomeIcon;
}

const BASE_LINKS: LinkDef[] = [
  { section: "home", href: "/", label: "Home", icon: HomeIcon },
  { section: "whatsNext", href: "/whats-next", label: "What's Next", icon: PaperPlaneIcon },
  { section: "editor", href: "/editor", label: "Editor", icon: Pencil2Icon },
  { section: "runs", href: "/runs", label: "Runs", icon: ListBulletIcon },
];

// deriveSection maps the current path to the highlighted nav entry by
// looking at the first path segment only — so `/runs/:id` highlights
// "runs" without `/boardroom` accidentally highlighting "board".
const SEGMENT_TO_SECTION: Record<string, Section> = {
  "whats-next": "whatsNext",
  editor: "editor",
  runs: "runs",
  // /insights is reached from the Runs list toolbar; highlight Runs
  // in the side nav so the operator's place stays legible even
  // though it's no longer a top-level entry.
  insights: "runs",
  board: "board",
  dispatcher: "dispatcher",
  triggers: "triggers",
  marketplace: "marketplace",
  plugins: "plugins",
  orgs: "org",
  teams: "team",
  integrations: "integrations",
  admin: "admin",
};

function deriveSection(pathname: string): Section | undefined {
  if (pathname === "/" || pathname === "") return "home";
  const segment = pathname.split("/")[1] ?? "";
  return SEGMENT_TO_SECTION[segment];
}

// NavLinks renders the primary navigation as a vertical column inside
// the Sidebar. When `collapsed` is true the labels are hidden and each
// link becomes a square icon button with a native tooltip. Under the
// Editor and Runs entries, an optional foldable sub-list shows the
// currently-open tabs of that kind so users can jump straight to a
// specific file/run without going through the section's inner strip.
export default function NavLinks({ collapsed }: Props) {
  const info = useServerInfoStore((s) => s.info);
  const { activeTeam, user } = useAuth();
  const [location] = useLocation();
  // Integrations is its own top-level route (/integrations, team-scoped via
  // the active team in context — like /board, /plugins), so deriveSection's
  // first-segment match keeps it highlighted across every integration sub-tab.
  const active = deriveSection(location);
  const alertUnseen = useUIStore((s) => s.alertUnseen);
  const clearAlertUnseen = useUIStore((s) => s.clearAlertUnseen);

  // Primary nav is organised into labelled groups so the rail reads as a
  // hierarchy instead of a flat list: Workspace (author + run), Operate
  // (the kanban / dispatch / automation control surfaces), and Extend
  // (marketplace + plugins). Manage (Integrations / Admin) only appears in
  // cloud mode. Each group's links are still gated by the same serverInfo
  // flags as before — empty groups drop out entirely.
  const operate: LinkDef[] = [];
  if (info?.native_tracker_enabled) {
    operate.push({ section: "board", href: "/board", label: "Board", icon: ViewGridIcon });
  }
  if (info?.dispatcher_enabled) {
    operate.push({ section: "dispatcher", href: "/dispatcher", label: "Dispatcher", icon: RocketIcon });
  }
  if (info?.triggers_enabled) {
    operate.push({ section: "triggers", href: "/triggers", label: "Automations", icon: LightningBoltIcon });
  }

  const extend: LinkDef[] = [];
  if (info?.marketplace_enabled) {
    extend.push({ section: "marketplace", href: "/marketplace", label: "Marketplace", icon: ArchiveIcon });
  }
  if (info?.plugins_enabled) {
    extend.push({ section: "plugins", href: "/plugins", label: "Plugins", icon: Component1Icon });
  }

  // Neither the org nor the team is a primary nav entry: the org lives in the
  // OrgSwitcher (top), team settings in the account chip ("Team settings").
  // Integrations stays — the valued "connect a repo / enable a bot" shortcut.
  // Both are cloud-only (the forge stores are wired only in cloud mode).
  const manage: LinkDef[] = [];
  if (activeTeam && info?.mode === "cloud") {
    manage.push({ section: "integrations", href: "/integrations", label: "Integrations", icon: Link2Icon });
  }
  // Admin: super-admins only, cloud mode only (/admin/orgs is a cloud-only
  // console whose API isn't registered in local/desktop mode).
  if (user?.is_super_admin && info?.mode === "cloud") {
    manage.push({ section: "admin", href: "/admin/orgs", label: "Admin", icon: GearIcon });
  }

  const groups: { label?: string; links: LinkDef[] }[] = [
    { links: BASE_LINKS },
    { label: "Operate", links: operate },
    { label: "Extend", links: extend },
    { label: "Manage", links: manage },
  ].filter((g) => g.links.length > 0);

  return (
    <nav className="flex flex-col gap-3" aria-label="Primary navigation">
      {groups.map((group, gi) => (
        <div key={group.label ?? "workspace"} className="flex flex-col gap-0.5">
          {group.label &&
            (collapsed ? (
              // Collapsed: a hairline divider stands in for the caption so
              // the grouping survives the icon-only rail.
              gi > 0 && (
                <div
                  className="mx-auto mb-1 h-px w-5 bg-border-default"
                  aria-hidden="true"
                />
              )
            ) : (
              <div className="px-2 pb-0.5 pt-1 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
                {group.label}
              </div>
            ))}
          {group.links.map(({ section, href, label, icon: Icon }) => {
            const isActive = active === section;
            const withSublist =
              !collapsed && (section === "editor" || section === "runs");
            // Run-health alert dot rides the Runs entry: an operator who
            // looked away sees a run needs attention. Acknowledged (cleared)
            // when they click into the Runs section.
            const showAlertDot = section === "runs" && alertUnseen > 0;
            return (
              <NavRow
                key={section}
                href={href}
                label={label}
                icon={Icon}
                isActive={isActive}
                collapsed={collapsed}
                sublistKind={withSublist ? section : null}
                showAlertDot={showAlertDot}
                onNavClick={section === "runs" ? clearAlertUnseen : undefined}
              />
            );
          })}
        </div>
      ))}
    </nav>
  );
}

interface NavRowProps {
  href: string;
  label: string;
  icon: typeof HomeIcon;
  isActive: boolean;
  collapsed: boolean;
  sublistKind: "editor" | "runs" | null;
  showAlertDot?: boolean;
  onNavClick?: () => void;
}

function NavRow({ href, label, icon: Icon, isActive, collapsed, sublistKind, showAlertDot, onNavClick }: NavRowProps) {
  const editorTabs = useTabsStore(useShallow(selectEditorTabs));
  const runTabs = useTabsStore(useShallow(selectRunTabs));
  const tabs =
    sublistKind === "editor"
      ? editorTabs
      : sublistKind === "runs"
        ? runTabs
        : [];
  const storageKey =
    sublistKind === "editor" ? EDITOR_FOLDED_KEY : RUNS_FOLDED_KEY;
  const [folded, setFolded] = useState(() =>
    sublistKind ? readBooleanFlag(storageKey) : true,
  );

  const base =
    "inline-flex items-center text-xs rounded-[var(--radius-md)] transition-[background-color,color] duration-[var(--motion-fast)] ease-[var(--motion-ease)]";
  const layout = collapsed ? "justify-center h-8 w-full" : "gap-2 px-2 py-1.5";
  const state = isActive
    ? "bg-accent-soft text-fg-default font-medium"
    : "text-fg-muted hover:bg-surface-2 hover:text-fg-default";

  const toggle = () => {
    setFolded((f) => {
      const next = !f;
      writeBooleanFlag(storageKey, next);
      return next;
    });
  };

  return (
    <>
      <div className={`${base} ${layout} ${state} ${collapsed ? "" : "flex"} relative`}>
        {/* Signature "you are here" indicator: a short accent bar pinned to
            the left edge of the active row (expanded only — the collapsed
            rail relies on the accent-soft fill + tinted icon instead). */}
        {isActive && !collapsed && (
          <span
            className="absolute left-0 top-1/2 h-4 w-[3px] -translate-y-1/2 rounded-full bg-accent"
            aria-hidden="true"
          />
        )}
        <Link
          href={href}
          className={`inline-flex items-center gap-2 min-w-0 flex-1 focus:outline-none ${collapsed ? "justify-center" : ""}`}
          aria-current={isActive ? "page" : undefined}
          title={showAlertDot ? `${label} — run needs attention` : label}
          aria-label={showAlertDot ? `${label}, run needs attention` : label}
          onClick={onNavClick}
        >
          <span className="relative inline-flex shrink-0">
            <Icon
              className={`w-3.5 h-3.5 shrink-0 ${isActive ? "text-accent-text" : ""}`}
            />
            {showAlertDot && (
              <span
                className="absolute -top-1 -right-1 w-2 h-2 rounded-full bg-danger ring-2 ring-surface-1"
                aria-hidden="true"
              />
            )}
          </span>
          {!collapsed && <span className="truncate">{label}</span>}
        </Link>
        {sublistKind && tabs.length > 0 && (
          <button
            type="button"
            onClick={toggle}
            className="ml-1 inline-flex items-center gap-0.5 text-caption text-fg-subtle hover:text-fg-default rounded px-1 py-0.5 hover:bg-surface-3"
            aria-expanded={!folded}
            title={folded ? `Show ${tabs.length} open tab${tabs.length === 1 ? "" : "s"}` : "Hide open tabs"}
          >
            <span>{tabs.length}</span>
            {folded ? (
              <ChevronRightIcon className="w-3 h-3" />
            ) : (
              <ChevronDownIcon className="w-3 h-3" />
            )}
          </button>
        )}
      </div>
      {sublistKind && !folded && tabs.length > 0 && (
        <SidebarTabList tabs={tabs} kind={sublistKind} />
      )}
    </>
  );
}

const RUNS_FOLDED_KEY = "iterion.sidebar.runsFolded";
const EDITOR_FOLDED_KEY = "iterion.sidebar.editorFolded";

function SidebarTabList({
  tabs,
  kind,
}: {
  tabs: Tab[];
  kind: "editor" | "runs";
}) {
  const [location] = useLocation();
  const icon =
    kind === "editor" ? (
      <Pencil2Icon className="w-3 h-3 shrink-0" />
    ) : (
      <PlayIcon className="w-3 h-3 shrink-0" />
    );
  return (
    <div className="flex flex-col gap-px ml-5 border-l border-border-default pl-1.5">
      {tabs.map((tab) => {
        const href =
          tab.kind === "run"
            ? `/runs/${encodeURIComponent(tab.params.runId ?? "")}`
            : tab.params.file
              ? `/editor?file=${encodeURIComponent(tab.params.file)}`
              : "/editor";
        return (
          <SidebarTabRow
            key={tab.id}
            tab={tab}
            href={href}
            currentLocation={location}
            icon={icon}
          />
        );
      })}
    </div>
  );
}

function SidebarTabRow({
  tab,
  href,
  currentLocation,
  icon,
}: {
  tab: Tab;
  href: string;
  currentLocation: string;
  icon: React.ReactNode;
}) {
  const closeTab = useTabsStore((s) => s.closeTab);
  const [, setLocation] = useLocation();
  const isActive = matchesCurrent(tab, currentLocation);

  const handleClose = (e: React.MouseEvent) => {
    e.stopPropagation();
    const wasViewing = matchesCurrent(tab, currentLocation);
    closeTab(tab.id);
    if (!wasViewing) return;
    // The URL still points at the just-closed tab; reroute to the
    // sibling tab the store fell back to (or the section's list view
    // when no tabs of this kind remain).
    const next = useTabsStore.getState();
    if (tab.kind === "run") {
      const fallback = next.tabs.find(
        (t) => t.id === next.activeRunTabId && t.kind === "run",
      );
      const fallbackRunId = fallback?.params.runId;
      setLocation(
        fallbackRunId ? `/runs/${encodeURIComponent(fallbackRunId)}` : "/runs",
        { replace: true },
      );
    } else if (tab.kind === "editor") {
      const fallback = next.tabs.find(
        (t) => t.id === next.activeEditorTabId && t.kind === "editor",
      );
      const fallbackFile = fallback?.params.file;
      setLocation(
        fallbackFile ? `/editor?file=${encodeURIComponent(fallbackFile)}` : "/editor",
        { replace: true },
      );
    }
  };

  return (
    <div
      className={`group inline-flex items-center gap-1 px-1.5 py-0.5 text-micro rounded ${
        isActive ? "bg-accent-soft text-fg-default" : "text-fg-muted hover:bg-surface-2 hover:text-fg-default"
      }`}
    >
      <Link href={href} className="inline-flex items-center gap-1 min-w-0 flex-1" title={tab.label}>
        {icon}
        <span className="truncate">{tab.label}</span>
      </Link>
      <button
        type="button"
        onClick={handleClose}
        className="inline-flex items-center justify-center w-4 h-4 rounded text-fg-subtle hover:text-fg-default hover:bg-surface-3 opacity-0 group-hover:opacity-100 focus:opacity-100"
        title="Close tab"
        aria-label={`Close tab ${tab.label}`}
      >
        <Cross2Icon className="w-2.5 h-2.5" />
      </button>
    </div>
  );
}

// matchesCurrent checks whether the current URL targets a specific tab.
// For run tabs we compare the path's runId; for editor tabs we compare
// the `?file=` search param. The URL→tab effect inside each section
// view keeps these in sync, so the highlight stays accurate without
// needing the active-tab id from the store.
function matchesCurrent(tab: Tab, location: string): boolean {
  if (tab.kind === "run") {
    const segments = location.split("?")[0]!.split("/");
    return segments[1] === "runs" && segments[2] === tab.params.runId;
  }
  if (tab.kind === "editor") {
    const [path, search = ""] = location.split("?");
    if (path !== "/editor") return false;
    const sp = new URLSearchParams(search);
    return (sp.get("file") ?? "") === (tab.params.file ?? "");
  }
  return false;
}
