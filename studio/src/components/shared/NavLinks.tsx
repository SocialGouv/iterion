import { useState } from "react";
import { Link, useLocation, useSearch } from "wouter";
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
  PersonIcon,
  Link2Icon,
  GearIcon,
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
  orgs: "org",
  teams: "team",
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
  const search = useSearch();
  let active = deriveSection(location);
  // The team page hosts the Integrations tab under ?tab=integrations; give
  // the dedicated Integrations nav entry its own highlight there so it and
  // the team entry don't both light up.
  if (active === "team" && new URLSearchParams(search).get("tab") === "integrations") {
    active = "integrations";
  }
  const alertUnseen = useUIStore((s) => s.alertUnseen);
  const clearAlertUnseen = useUIStore((s) => s.clearAlertUnseen);

  const links: LinkDef[] = [...BASE_LINKS];
  if (info?.native_tracker_enabled) {
    links.push({ section: "board", href: "/board", label: "Board", icon: ViewGridIcon });
  }
  if (info?.dispatcher_enabled) {
    links.push({ section: "dispatcher", href: "/dispatcher", label: "Dispatcher", icon: RocketIcon });
  }
  if (info?.triggers_enabled) {
    links.push({ section: "triggers", href: "/triggers", label: "Automations", icon: LightningBoltIcon });
  }
  if (info?.marketplace_enabled) {
    links.push({ section: "marketplace", href: "/marketplace", label: "Marketplace", icon: ArchiveIcon });
  }
  // Org settings hub now lives in the dedicated OrgSwitcher at the top of the
  // sidebar (switch org + "Organization settings"); it's intentionally NOT
  // duplicated as a primary nav entry here — that double-listed the org name
  // (once as the org, once as the same-named default team).
  // Team entry hidden when no team is active (e.g. desktop / local mode).
  if (activeTeam) {
    links.push({
      section: "team",
      href: `/teams/${activeTeam.team_id}`,
      label: activeTeam.team_name || "Team",
      icon: PersonIcon,
    });
    // Integrations: connect a GitLab/GitHub/Forgejo repo and enable a bot on
    // it (the team page's Integrations tab). Cloud-only (the forge stores are
    // wired only in cloud mode); surfaced as its own top-level entry because
    // "connect a repo to Revi" is otherwise buried two clicks deep under the
    // team-name link.
    if (info?.mode === "cloud") {
      links.push({
        section: "integrations",
        href: `/teams/${activeTeam.team_id}?tab=integrations`,
        label: "Integrations",
        icon: Link2Icon,
      });
    }
  }
  // Admin section: super-admins, and only in cloud mode. /admin/orgs is the
  // org/tenant console, a cloud-only concept whose API isn't registered in
  // local/desktop mode — surfacing the link there leads to a dead page.
  if (user?.is_super_admin && info?.mode === "cloud") {
    links.push({ section: "admin", href: "/admin/orgs", label: "Admin", icon: GearIcon });
  }

  return (
    <nav className="flex flex-col gap-0.5" aria-label="Primary navigation">
      {links.map(({ section, href, label, icon: Icon }) => {
        const isActive = active === section;
        const withSublist =
          !collapsed && (section === "editor" || section === "runs");
        // Run-health alert dot rides the Runs entry: an operator who
        // looked away sees a run needs attention. Acknowledged (cleared)
        // when they click into the Runs section.
        const showAlertDot = section === "runs" && alertUnseen > 0;
        return (
          <div key={section} className="flex flex-col gap-0.5">
            <NavRow
              href={href}
              label={label}
              icon={Icon}
              isActive={isActive}
              collapsed={collapsed}
              sublistKind={withSublist ? section : null}
              showAlertDot={showAlertDot}
              onNavClick={section === "runs" ? clearAlertUnseen : undefined}
            />
          </div>
        );
      })}
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

  const base = "inline-flex items-center text-xs rounded border transition-colors";
  const layout = collapsed ? "justify-center h-8 w-full" : "gap-2 px-2 py-1.5";
  const state = isActive
    ? "border-accent/40 bg-accent-soft text-fg-default"
    : "border-transparent text-fg-muted hover:bg-surface-2 hover:text-fg-default";

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
        <Link
          href={href}
          className={`inline-flex items-center gap-2 min-w-0 flex-1 focus:outline-none ${collapsed ? "justify-center" : ""}`}
          aria-current={isActive ? "page" : undefined}
          title={showAlertDot ? `${label} — run needs attention` : label}
          aria-label={showAlertDot ? `${label}, run needs attention` : label}
          onClick={onNavClick}
        >
          <span className="relative inline-flex shrink-0">
            <Icon className="w-3.5 h-3.5 shrink-0" />
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
