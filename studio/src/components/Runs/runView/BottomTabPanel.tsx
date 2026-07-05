import { Tabs } from "@/components/ui";
import { type RunEvent } from "@/api/runs";

import ArtifactsPanel from "../artifacts/ArtifactsPanel";
import PlansPanel from "../PlansPanel";
import BrowserPane, { type BrowserDock } from "../BrowserPane";
import EventLog from "../EventLog";
import ReportTab from "../ReportTab";
import RunLogPanel from "../RunLogPanel";
import { SessionBoardTab } from "../sessionboard/SessionBoardTab";

import { BOTTOM_TABS, BOTTOM_TAB_LABELS, type BottomTab } from "./layoutFlags";

// Bottom-drawer JSX for the run console: a Tabs strip plus the panel
// for the currently-selected tab. Lifted out of RunView to keep the
// host's render tree readable. Pure presentational — props in, no
// state ownership.
interface BottomTabPanelProps {
  runId: string;
  bottomTab: BottomTab;
  onSelectTab: (tab: BottomTab) => void;
  browserAvailable: boolean;
  browserRightDocked: boolean;
  browserDock: BrowserDock;
  setBrowserDock: (next: BrowserDock) => void;
  scrubSeq: number | null;
  scrubbing: boolean;
  followTail: boolean;
  setFollowTail: (next: boolean) => void;
  displayedEvents: RunEvent[];
  eventLogSelection: string | null;
  onEventSelect: (nodeId: string, iteration: number) => void;
  onClearSelection: () => void;
  onCollapse: () => void;
  onSelectNode: (nodeId: string | null) => void;
  subscribeLogs: (fromOffset?: number) => void;
  unsubscribeLogs: () => void;
  logClampBytes: number | null | undefined;
}

export function BottomTabPanel(props: BottomTabPanelProps) {
  const {
    runId,
    bottomTab,
    onSelectTab,
    browserAvailable,
    browserRightDocked,
    browserDock,
    setBrowserDock,
    scrubSeq,
    scrubbing,
    followTail,
    setFollowTail,
    displayedEvents,
    eventLogSelection,
    onEventSelect,
    onClearSelection,
    onCollapse,
    onSelectNode,
    subscribeLogs,
    unsubscribeLogs,
    logClampBytes,
  } = props;
  return (
    <div className="h-full border-t border-border-default min-h-0 overflow-hidden animate-fade-in-opacity flex flex-col bg-surface-1">
      <Tabs
        value={bottomTab}
        onValueChange={(v) => onSelectTab(v as BottomTab)}
        items={BOTTOM_TABS.filter(
          (t) => t !== "browser" || (browserAvailable && !browserRightDocked),
        ).map((t) => ({ value: t, label: BOTTOM_TAB_LABELS[t] }))}
        variant="underline"
        listClassName="px-3"
      />
      <div className="flex-1 min-h-0">
        {bottomTab === "tasks" ? (
          <SessionBoardTab runId={runId} />
        ) : bottomTab === "events" ? (
          <EventLog
            events={displayedEvents}
            selectedExecutionId={eventLogSelection}
            followTail={followTail && !scrubbing}
            onToggleFollow={setFollowTail}
            onSelectNodeIteration={onEventSelect}
            onClearSelection={onClearSelection}
            onCollapse={onCollapse}
            runId={runId}
          />
        ) : bottomTab === "logs" ? (
          <RunLogPanel
            runId={runId}
            subscribeLogs={subscribeLogs}
            unsubscribeLogs={unsubscribeLogs}
            onCollapse={onCollapse}
            clampToBytes={logClampBytes}
          />
        ) : bottomTab === "browser" && runId ? (
          <BrowserPane
            runId={runId}
            scrubSeq={scrubSeq}
            dock={browserDock}
            onDockChange={setBrowserDock}
          />
        ) : bottomTab === "artifacts" ? (
          <ArtifactsPanel runId={runId} />
        ) : bottomTab === "plans" ? (
          <PlansPanel runId={runId} />
        ) : (
          <ReportTab onSelectNode={onSelectNode} />
        )}
      </div>
    </div>
  );
}

