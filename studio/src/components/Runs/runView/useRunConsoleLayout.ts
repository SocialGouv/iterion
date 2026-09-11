import {
  type Dispatch,
  type SetStateAction,
  useCallback,
  useState,
} from "react";

import {
  hasFlag,
  readBooleanFlag,
  readEnumFlag,
  removeFlag,
  writeBooleanFlag,
  writeStringFlag,
} from "@/lib/localStorageFlag";

import { type BrowserDock } from "../BrowserPane";
import {
  BOTTOM_TABS,
  BOTTOM_TAB_KEY,
  DETAIL_COLLAPSED_KEY,
  EVENTLOG_COLLAPSED_KEY,
  readBrowserDock,
  writeBrowserDock,
  type BottomTab,
} from "./layoutFlags";

export interface RunConsoleLayout {
  browserDock: BrowserDock;
  setBrowserDock: (next: BrowserDock) => void;
  detailCollapsed: boolean;
  toggleDetailCollapsed: () => void;
  eventlogCollapsed: boolean;
  toggleEventlogCollapsed: () => void;
  setEventlogCollapsed: Dispatch<SetStateAction<boolean>>;
  bottomTab: BottomTab;
  setBottomTab: Dispatch<SetStateAction<BottomTab>>;
  handleSetBottomTab: (tab: BottomTab) => void;
  bottomTabPinned: boolean;
  setBottomTabPinned: Dispatch<SetStateAction<boolean>>;
  resetLayout: () => void;
}

// Owns the run console's persisted layout dials — browser dock, detail +
// event-log collapse, the bottom tab and its "pinned" flag. Steering is a
// permanent right dock and therefore has no persisted presentation state.
// This is pure UI-persistence state (localStorage-backed); the cross-cutting effects
// that *react* to run data (auto-reveal Browser on first preview_url, the
// "Show event log" token, the browserDock→bottomTab redirect) stay in
// RunView and drive the raw setters this hook exposes.
export function useRunConsoleLayout(): RunConsoleLayout {
  const [browserDock, setBrowserDockState] = useState<BrowserDock>(() =>
    readBrowserDock(),
  );
  const setBrowserDock = useCallback((next: BrowserDock) => {
    setBrowserDockState(next);
    writeBrowserDock(next);
  }, []);

  // Canvas-first defaults: node-detail starts collapsed so the canvas
  // claims the full width on first render; the bottom events/logs
  // drawer stays open because it carries actionable signal at every
  // run state (queued progress, live tool output, post-mortem report).
  const [detailCollapsed, setDetailCollapsed] = useState<boolean>(() =>
    readBooleanFlag(DETAIL_COLLAPSED_KEY, true),
  );
  const [eventlogCollapsed, setEventlogCollapsed] = useState<boolean>(() =>
    readBooleanFlag(EVENTLOG_COLLAPSED_KEY, false),
  );
  const [bottomTab, setBottomTab] = useState<BottomTab>(() =>
    readEnumFlag(BOTTOM_TAB_KEY, BOTTOM_TABS, "logs"),
  );
  // Tracks whether the user has manually changed the bottom tab during
  // this run view, so we don't yank the tab back to "browser" on every
  // new preview_url event after they explicitly picked another panel.
  // A persisted tab counts as "pinned" — the user chose it last time.
  const [bottomTabPinned, setBottomTabPinned] = useState<boolean>(() => {
    return hasFlag(BOTTOM_TAB_KEY);
  });
  const handleSetBottomTab = useCallback((tab: BottomTab) => {
    setBottomTab(tab);
    setBottomTabPinned(true);
    writeStringFlag(BOTTOM_TAB_KEY, tab);
  }, []);
  const toggleDetailCollapsed = useCallback(() => {
    setDetailCollapsed((prev) => {
      const next = !prev;
      writeBooleanFlag(DETAIL_COLLAPSED_KEY, next);
      return next;
    });
  }, []);
  const toggleEventlogCollapsed = useCallback(() => {
    setEventlogCollapsed((prev) => {
      const next = !prev;
      writeBooleanFlag(EVENTLOG_COLLAPSED_KEY, next);
      return next;
    });
  }, []);

  // Restore every dock/collapse dial to its first-run default and clear
  // the persisted flags. Pairs with the host clearing the panel-size
  // layouts (useLayoutPersistence.reset) for a full "reset layout".
  const resetLayout = useCallback(() => {
    setBrowserDock("bottom");
    setDetailCollapsed(true);
    writeBooleanFlag(DETAIL_COLLAPSED_KEY, true);
    setEventlogCollapsed(false);
    writeBooleanFlag(EVENTLOG_COLLAPSED_KEY, false);
    setBottomTab("logs");
    setBottomTabPinned(false);
    removeFlag(BOTTOM_TAB_KEY);
  }, [setBrowserDock]);

  return {
    browserDock,
    setBrowserDock,
    detailCollapsed,
    toggleDetailCollapsed,
    eventlogCollapsed,
    toggleEventlogCollapsed,
    setEventlogCollapsed,
    bottomTab,
    setBottomTab,
    handleSetBottomTab,
    bottomTabPinned,
    setBottomTabPinned,
    resetLayout,
  };
}
