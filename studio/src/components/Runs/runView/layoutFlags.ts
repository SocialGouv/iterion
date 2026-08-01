import { DOCK_STATES } from "@/lib/chatDock/dockState";
import { readEnumFlag, writeStringFlag } from "@/lib/localStorageFlag";

import { type BrowserDock } from "../BrowserPane";

// Layout/panel preferences are namespaced under `run-console-v2.*`.
// Never reuse `run-console-v1.*` keys — the layout shape changed
// (no global view mode, chat is a separate panel) and mixing them
// produces inconsistent restored state.
export const DETAIL_COLLAPSED_KEY = "run-console-v2.detail-collapsed";
export const EVENTLOG_COLLAPSED_KEY = "run-console-v2.eventlog-collapsed";
export const BOTTOM_TAB_KEY = "run-console-v2.bottom-tab";
export const BOTTOM_TABS = [
  "tasks",
  "events",
  "logs",
  "report",
  "browser",
  "artifacts",
  "plans",
] as const;
export type BottomTab = (typeof BOTTOM_TABS)[number];
// The run console's own (steering) dock keeps its historical key: it is
// independent of the shell-level assistant dock, which persists under
// ASSISTANT_DOCK_KEY. Both share one state vocabulary.
export const CHAT_DOCK_KEY = "run-console-v2.chat-dock";
export const CHAT_DOCKS = DOCK_STATES;
export const BOTTOM_TAB_LABELS: Record<BottomTab, string> = {
  tasks: "Tasks",
  events: "Events",
  logs: "Logs",
  report: "Report",
  browser: "Browser",
  artifacts: "Artifacts",
  plans: "Plans",
};
export const BROWSER_DOCK_KEY = "run-console-v2.browser-dock";

export function readBrowserDock(): BrowserDock {
  return readEnumFlag<BrowserDock>(BROWSER_DOCK_KEY, ["right", "bottom"], "bottom");
}

export function writeBrowserDock(dock: BrowserDock): void {
  writeStringFlag(BROWSER_DOCK_KEY, dock);
}
