import { useEffect, useMemo } from "react";
import { useLocation } from "wouter";

import CommandPalette, {
  type CommandAction,
} from "@/components/shared/CommandPalette";
import { forgeTeamRepoKey } from "@/api/forgeConnections";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useRuns } from "@/hooks/useRuns";
import { useRecentsStore } from "@/store/recents";
import { useServerInfoStore } from "@/store/serverInfo";
import { useThemeStore } from "@/store/theme";
import { useUIStore } from "@/store/ui";

// GlobalCommandPalette is the route-agnostic Cmd+K palette. It mounts
// in App and surfaces navigation, recent runs, recent files, and the
// theme toggle. The editor route owns its own Canvas-scoped palette
// (Canvas.tsx) because those actions depend on canvas-local handlers
// (undo/redo wired to React Flow, fitView, layer toggles, …); App
// suppresses its own listener while the editor is mounted to avoid
// double-firing on Cmd+K.
export default function GlobalCommandPalette() {
  const [location, setLocation] = useLocation();
  const open = useUIStore((s) => s.commandPaletteOpen);
  const setOpen = useUIStore((s) => s.setCommandPaletteOpen);
  const recents = useRecentsStore((s) => s.recents);
  const { runs } = useRuns({ limit: 5, enabled: open });
  const serverInfo = useServerInfoStore((s) => s.info);
  const cloud = serverInfo?.mode === "cloud";
  const cycleTheme = useThemeStore((s) => s.cycleMode);
  // useActiveRepo already backs the sidebar RepoSwitcher (cached by TanStack),
  // so reading it here piggybacks on that query — no extra fetch on Cmd+K.
  const { repos, choose, enabled: cloudRepos } = useActiveRepo();

  // The Canvas Cmd+K listener handles the editor route. Everywhere
  // else, App intercepts Cmd+K and opens this palette.
  const inEditor = location === "/editor";

  useEffect(() => {
    if (inEditor) return;
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === "k" || e.key === "K")) {
        e.preventDefault();
        setOpen(!useUIStore.getState().commandPaletteOpen);
        const target = e.target as HTMLElement | null;
        if (target && typeof target.blur === "function") target.blur();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [inEditor, setOpen]);

  const actions: CommandAction[] = useMemo(
    () => [
      {
        id: "nav.home",
        group: "Navigate",
        title: "Home",
        keywords: ["start", "projects"],
        run: () => setLocation("/"),
      },
      {
        id: "nav.editor",
        group: "Navigate",
        title: "Editor",
        keywords: ["canvas", "design", "edit"],
        // Cloud has no editor save path (the Editor is out of the cloud nav).
        disabled: cloud,
        run: () => setLocation("/editor"),
      },
      {
        id: "nav.runs",
        group: "Navigate",
        title: "Runs",
        keywords: ["history", "list", "console"],
        run: () => setLocation("/runs"),
      },
      {
        id: "nav.board",
        group: "Navigate",
        title: "Board",
        keywords: ["kanban", "issues"],
        disabled: !serverInfo?.native_tracker_enabled,
        run: () => setLocation("/board"),
      },
      {
        id: "nav.pipelines",
        group: "Navigate",
        title: "Pipelines",
        keywords: ["pipeline", "boards", "human input", "bots"],
        disabled: !serverInfo?.native_tracker_enabled,
        run: () => setLocation("/pipelines"),
      },
      {
        id: "nav.dispatcher",
        group: "Navigate",
        title: "Dispatcher",
        keywords: ["retries", "queue"],
        disabled: !serverInfo?.dispatcher_enabled,
        run: () => setLocation("/dispatcher"),
      },
      {
        id: "nav.whats-next",
        group: "Navigate",
        title: "What's Next",
        keywords: ["nexie", "co-cto", "chat"],
        run: () => setLocation("/whats-next"),
      },
      {
        id: "nav.automations",
        group: "Navigate",
        title: "Automations",
        keywords: ["triggers", "cron", "schedule", "webhooks"],
        disabled: !serverInfo?.triggers_enabled,
        run: () => setLocation("/triggers"),
      },
      {
        id: "nav.insights",
        group: "Navigate",
        title: "Insights",
        keywords: ["analytics", "cost", "stats"],
        run: () => setLocation("/insights"),
      },
      {
        id: "nav.marketplace",
        group: "Navigate",
        title: "Marketplace",
        keywords: ["bots", "install", "registry"],
        disabled: !serverInfo?.marketplace_enabled,
        run: () => setLocation("/marketplace"),
      },
      {
        id: "nav.plugins",
        group: "Navigate",
        title: "Plugins",
        keywords: ["rewriter", "mcp", "rtk"],
        disabled: !serverInfo?.plugins_enabled,
        run: () => setLocation("/plugins"),
      },
      {
        id: "nav.skills",
        group: "Navigate",
        title: "Skills",
        keywords: ["library", "skill.md"],
        disabled: !serverInfo?.skills_enabled,
        run: () => setLocation("/skills"),
      },
      {
        id: "nav.secrets",
        group: "Navigate",
        title: "Secrets",
        keywords: ["credentials", "keys", "byok"],
        disabled: !serverInfo?.secrets_enabled,
        run: () => setLocation("/secrets"),
      },
      {
        id: "theme.cycle",
        group: "View",
        title: "Cycle theme (system / light / dark)",
        keywords: ["dark", "light", "appearance"],
        run: cycleTheme,
      },
      ...runs.slice(0, 5).map<CommandAction>((r) => ({
        id: `runs.recent.${r.id}`,
        group: "Recent runs",
        title: r.name || r.workflow_name,
        keywords: [r.id, r.workflow_name, r.file_path ?? ""],
        run: () => setLocation(`/runs/${encodeURIComponent(r.id)}`),
      })),
      // Cloud-only repo actions — mirror the sidebar RepoSwitcher so the
      // palette becomes a keyboard-first shortcut to the same context switch.
      ...(cloudRepos
        ? [
            {
              id: "repo.connect",
              group: "Repositories",
              title: "Connect a repository…",
              keywords: ["forge", "github", "gitlab", "integration", "add"],
              run: () => setLocation("/integrations/connect"),
            } as CommandAction,
            {
              id: "repo.all",
              group: "Repositories",
              title: "Switch to all repos",
              keywords: ["overview", "aggregate"],
              run: () => choose(null),
            } as CommandAction,
            ...repos.map<CommandAction>((r) => {
              const key = forgeTeamRepoKey(r);
              return {
                id: `repo.switch.${key}`,
                group: "Repositories",
                title: `Switch to ${r.repo_full_name}`,
                keywords: [r.provider, r.repo_full_name],
                run: () => choose(key),
              };
            }),
          ]
        : []),
      // Local/desktop-only: recent .bot file paths land in LaunchView. Cloud
      // has no host filesystem, so /runs/new?file=<abs path> would 404.
      ...(cloud
        ? []
        : recents.slice(0, 5).map<CommandAction>((path) => ({
            id: `files.recent.${path}`,
            group: "Recent files",
            title: path,
            run: () => setLocation(`/runs/new?file=${encodeURIComponent(path)}`),
          }))),
    ],
    [runs, recents, serverInfo, setLocation, cycleTheme, cloud, cloudRepos, repos, choose],
  );

  return (
    <CommandPalette open={open} actions={actions} onClose={() => setOpen(false)} />
  );
}
