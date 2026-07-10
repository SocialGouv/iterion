import { lazy, Suspense, useCallback, useEffect, useRef, useState } from "react";

import { desktop, isCloudConnection, onDesktopEvent, type Project } from "@/lib/desktopBridge";
import { DesktopEvent } from "@/lib/desktopEvents";
import { showRunAlertNotification, type RunAlertPayload } from "@/lib/desktopNotify";

const CloudConnectModal = lazy(() => import("@/views/ProjectSwitcher/CloudConnectModal"));
const CloudReloginModal = lazy(() => import("@/components/shared/CloudReloginModal"));
const Welcome = lazy(() => import("@/views/Welcome"));

/**
 * WorkspaceShell is the desktop's top-level chrome in multi-connection mode.
 * It renders one live pane per OPEN connection — each an <iframe src="/x/<id>/">
 * loading the studio SPA scoped to that backend through the demux asset proxy —
 * and a tab bar to focus / split / open / close them. Local and cloud panes are
 * shown side by side: connecting to the cloud no longer hides the local runs.
 *
 * The shell OWNS the Wails native bindings (add project, connect cloud, open /
 * close connection); the panes never touch window.go (their IPC callbacks
 * wouldn't resolve inside an iframe). Panes stay MOUNTED while open — hidden via
 * CSS, not unmounted — so background runs keep streaming and a tab switch is
 * instant with no reload or re-login.
 */
export default function WorkspaceShell() {
  const [connections, setConnections] = useState<Project[]>([]);
  const [openIds, setOpenIds] = useState<string[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  const [splitId, setSplitId] = useState<string | null>(null);
  const [cloudOpen, setCloudOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  // A cloud pane's session expired (cloud:auth-expired) — the main frame owns
  // the token jars, so re-login is prompted here, not inside the pane.
  const [reloginConnId, setReloginConnId] = useState<string | null>(null);
  // firstRunPending: null = not yet probed, true = show onboarding, false = go.
  const [firstRunPending, setFirstRunPending] = useState<boolean | null>(null);
  const bootstrapped = useRef(false);
  // Live refs read by the native-event handlers (which subscribe once) so a
  // menu forward / focus always targets the CURRENT active pane iframe.
  const iframeRefs = useRef<Record<string, HTMLIFrameElement | null>>({});
  const activeIdRef = useRef<string | null>(null);
  activeIdRef.current = activeId;

  const loadConnections = useCallback(async () => {
    try {
      setConnections(await desktop.listConnections());
    } catch (err) {
      console.error("[workspace] listConnections failed", err);
    }
  }, []);

  // openPane activates the backend (spawn local daemon / hydrate cloud jar)
  // BEFORE mounting the iframe, so /x/<id>/ resolves in the demux proxy.
  const openPane = useCallback(async (id: string) => {
    try {
      await desktop.openConnection(id);
    } catch (err) {
      console.error("[workspace] openConnection failed", err);
      return;
    }
    setOpenIds((prev) => (prev.includes(id) ? prev : [...prev, id]));
    setActiveId(id);
    setMenuOpen(false);
  }, []);

  const closePane = useCallback(
    (id: string) => {
      void desktop.closeConnection(id).catch(() => {});
      setOpenIds((prev) => prev.filter((x) => x !== id));
      setSplitId((s) => (s === id ? null : s));
      setActiveId((a) => {
        if (a !== id) return a;
        const rest = openIds.filter((x) => x !== id);
        return rest[rest.length - 1] ?? null;
      });
    },
    [openIds],
  );

  // Bootstrap: load the connection list + whatever the Go registry already has
  // open (the startup-registered current connection), opening the first one if
  // nothing is live yet.
  useEffect(() => {
    if (bootstrapped.current) return;
    bootstrapped.current = true;
    void (async () => {
      // First-run onboarding takes over the whole shell until the wizard
      // marks itself done (API keys / CLI checks / first project).
      const pending = await desktop.isFirstRunPending().catch(() => false);
      if (pending) {
        setFirstRunPending(true);
        return;
      }
      setFirstRunPending(false);
      await loadConnections();
      let open: string[] = [];
      try {
        open = await desktop.getOpenConnections();
      } catch {
        open = [];
      }
      if (open.length === 0) {
        const first = (await desktop.listConnections().catch(() => [] as Project[]))[0];
        if (first) {
          await openPane(first.id);
          return;
        }
      }
      setOpenIds(open);
      setActiveId((a) => a ?? open[0] ?? null);
    })();
  }, [loadConnections, openPane]);

  // Refresh the connection list when it mutates (add/remove/cloud connect).
  useEffect(() => onDesktopEvent(DesktopEvent.ProjectsChanged, () => void loadConnections()), [loadConnections]);

  // Main-frame native wiring the shell owns (the panes can't — no window.go):
  // cloud re-login prompt, native run-health notifications, and the native
  // menu bar. Connection-scoped menu items (Settings / About / Undo / Redo)
  // are FORWARDED to the active pane's iframe via postMessage (its own SPA
  // handles them); shell-scoped items (New / Switch Project) act here.
  useEffect(() => {
    const forwardMenu = (menu: string, tab?: string) => {
      const id = activeIdRef.current;
      const el = id ? iframeRefs.current[id] : null;
      el?.contentWindow?.postMessage({ source: "iterion-shell", type: "menu", menu, tab }, "*");
    };
    const offs = [
      onDesktopEvent<string>(DesktopEvent.CloudAuthExpired, (connId) => setReloginConnId(connId)),
      onDesktopEvent<RunAlertPayload>(DesktopEvent.RunAlert, (payload) => showRunAlertNotification(payload)),
      onDesktopEvent(DesktopEvent.MenuNewProject, () => void onAddLocal()),
      onDesktopEvent(DesktopEvent.MenuSwitchProject, () => setMenuOpen(true)),
      onDesktopEvent(DesktopEvent.MenuSettings, () => forwardMenu("settings", "api-keys")),
      onDesktopEvent(DesktopEvent.MenuAbout, () => forwardMenu("settings", "about")),
      onDesktopEvent(DesktopEvent.MenuUndo, () => forwardMenu("undo")),
      onDesktopEvent(DesktopEvent.MenuRedo, () => forwardMenu("redo")),
    ];
    // A pane whose cloud session expired asks the shell (its parent) to prompt
    // re-login — the pane can't (no window.go), so it postMessages up here.
    const onPaneMessage = (e: MessageEvent) => {
      const d = e.data as { source?: string; type?: string; connId?: string } | null;
      if (d?.source === "iterion-pane" && d.type === "auth-expired" && d.connId) {
        setReloginConnId(d.connId);
      }
    };
    window.addEventListener("message", onPaneMessage);
    return () => {
      offs.forEach((off) => off());
      window.removeEventListener("message", onPaneMessage);
    };
    // onAddLocal is stable (deps are stable callbacks); the handlers read live
    // refs, so subscribing once avoids churning the native listeners.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const projectById = (id: string) => connections.find((p) => p.id === id);
  const notOpen = connections.filter((p) => !openIds.includes(p.id));
  const visibleIds = splitId ? [activeId, splitId].filter(Boolean) as string[] : activeId ? [activeId] : [];

  const onAddLocal = useCallback(async () => {
    setMenuOpen(false);
    try {
      const dir = await desktop.pickProjectDirectory();
      if (!dir) return;
      const p = await desktop.addProjectSilently(dir);
      await loadConnections();
      await openPane(p.id);
    } catch (err) {
      console.error("[workspace] add local project failed", err);
    }
  }, [loadConnections, openPane]);

  // Still probing first-run — hold on a neutral loader so the empty workspace
  // ("No connection open") never flashes before the initial pane opens.
  if (firstRunPending === null) {
    return <div className="h-screen flex items-center justify-center bg-surface-0 text-fg-muted">Loading…</div>;
  }

  // First-run onboarding takes the whole window (native bindings live in this
  // main frame, so the wizard runs here, not in a pane).
  if (firstRunPending) {
    return (
      <Suspense fallback={<div className="h-screen flex items-center justify-center text-fg-muted">Loading…</div>}>
        <Welcome
          onComplete={() => {
            setFirstRunPending(false);
            void (async () => {
              await loadConnections();
              const first = (await desktop.listConnections().catch(() => [] as Project[]))[0];
              if (first) await openPane(first.id);
            })();
          }}
        />
      </Suspense>
    );
  }

  return (
    <div className="h-screen w-screen flex flex-col bg-surface-0 text-fg-default overflow-hidden">
      {/* Tab bar */}
      <div className="flex items-center gap-1 h-10 px-2 border-b border-border-default bg-surface-1 shrink-0 select-none">
        {openIds.map((id) => {
          const p = projectById(id);
          const name = p?.name ?? id.slice(0, 8);
          const isActive = activeId === id;
          const inSplit = splitId === id;
          return (
            <div
              key={id}
              className={`group flex items-center gap-1.5 h-7 pl-2.5 pr-1 rounded-t text-xs cursor-pointer border-x border-t ${
                isActive || inSplit
                  ? "bg-surface-0 border-border-default"
                  : "bg-surface-2 border-transparent hover:bg-surface-0/60"
              }`}
              onClick={() => setActiveId(id)}
              title={p ? (isCloudConnection(p) ? p.cloud_url : p.dir) : id}
            >
              <span className={`w-1.5 h-1.5 rounded-full ${p && isCloudConnection(p) ? "bg-accent" : "bg-fg-subtle"}`} />
              <span className="max-w-40 truncate font-medium">{name}</span>
              {p && isCloudConnection(p) && (
                <span className="text-caption uppercase tracking-wider text-fg-subtle">cloud</span>
              )}
              <button
                type="button"
                aria-label={`Close ${name}`}
                className="ml-1 w-4 h-4 flex items-center justify-center rounded text-fg-subtle opacity-0 group-hover:opacity-100 hover:bg-surface-2 hover:text-fg-default"
                onClick={(e) => {
                  e.stopPropagation();
                  closePane(id);
                }}
              >
                ×
              </button>
            </div>
          );
        })}

        {/* + open connection menu */}
        <div className="relative">
          <button
            type="button"
            className="h-7 px-2 rounded text-fg-subtle hover:bg-surface-2 hover:text-fg-default text-sm"
            onClick={() => setMenuOpen((o) => !o)}
            aria-label="Open a connection"
          >
            +
          </button>
          {menuOpen && (
            <div className="absolute z-20 top-8 left-0 w-64 rounded border border-border-default bg-surface-0 shadow-lg py-1 text-xs">
              {notOpen.length > 0 && (
                <div className="px-2 py-1 text-caption uppercase tracking-wider text-fg-subtle">Open connection</div>
              )}
              {notOpen.map((p) => (
                <button
                  key={p.id}
                  type="button"
                  className="w-full text-left px-3 py-1.5 hover:bg-surface-2 flex items-center gap-2"
                  onClick={() => void openPane(p.id)}
                >
                  <span className={`w-1.5 h-1.5 rounded-full ${isCloudConnection(p) ? "bg-accent" : "bg-fg-subtle"}`} />
                  <span className="truncate flex-1">{p.name}</span>
                  {isCloudConnection(p) && <span className="text-caption uppercase text-fg-subtle">cloud</span>}
                </button>
              ))}
              <div className="my-1 border-t border-border-default" />
              <button type="button" className="w-full text-left px-3 py-1.5 hover:bg-surface-2" onClick={() => void onAddLocal()}>
                + Add local project…
              </button>
              <button
                type="button"
                className="w-full text-left px-3 py-1.5 hover:bg-surface-2"
                onClick={() => {
                  setMenuOpen(false);
                  setCloudOpen(true);
                }}
              >
                Connect to Cloud…
              </button>
            </div>
          )}
        </div>

        <div className="flex-1" />

        {/* Split toggle: show the active pane beside the next open one. */}
        {openIds.length >= 2 && (
          <button
            type="button"
            className={`h-7 px-2 rounded text-xs ${splitId ? "bg-accent/20 text-accent-text" : "text-fg-subtle hover:bg-surface-2 hover:text-fg-default"}`}
            onClick={() =>
              setSplitId((s) => {
                if (s) return null;
                const other = openIds.find((x) => x !== activeId);
                return other ?? null;
              })
            }
            title={splitId ? "Exit split view" : "Split view (show two connections side by side)"}
          >
            {splitId ? "Unsplit" : "Split"}
          </button>
        )}
      </div>

      {/* Panes: every open connection stays mounted (hidden panes keep their
          runs streaming); layout shows the active pane, or two in split. */}
      <div className="flex-1 flex min-h-0">
        {openIds.length === 0 && (
          <div className="flex-1 flex flex-col items-center justify-center gap-3 text-fg-muted">
            <p className="text-sm">No connection open.</p>
            <div className="flex gap-2">
              <button type="button" className="px-3 py-1.5 rounded border border-border-default hover:bg-surface-2 text-sm" onClick={() => void onAddLocal()}>
                Add local project…
              </button>
              <button type="button" className="px-3 py-1.5 rounded border border-border-default hover:bg-surface-2 text-sm" onClick={() => setCloudOpen(true)}>
                Connect to Cloud…
              </button>
            </div>
          </div>
        )}
        {openIds.map((id) => {
          const visible = visibleIds.includes(id);
          return (
            <iframe
              key={id}
              ref={(el) => {
                iframeRefs.current[id] = el;
              }}
              src={`/x/${id}/`}
              title={projectById(id)?.name ?? id}
              className="h-full w-full border-0 min-w-0"
              style={{ display: visible ? "block" : "none" }}
            />
          );
        })}
      </div>

      <Suspense fallback={null}>
        <CloudConnectModal
          open={cloudOpen}
          onClose={() => setCloudOpen(false)}
          onConnected={() => {
            setCloudOpen(false);
            void loadConnections();
          }}
        />
        <CloudReloginModal connId={reloginConnId} onClose={() => setReloginConnId(null)} />
      </Suspense>
    </div>
  );
}
