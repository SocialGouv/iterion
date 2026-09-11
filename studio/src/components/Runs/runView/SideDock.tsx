import { useEffect, useState } from "react";

import { Tabs } from "@/components/ui";

import BrowserPane, { type BrowserDock } from "../BrowserPane";
import { SteeringPanel } from "../SteeringPanel";

interface SideDockProps {
  runId: string;
  browserRightDocked: boolean;
  scrubSeq: number | null;
  browserDock: BrowserDock;
  onBrowserDockChange: (next: BrowserDock) => void;
  chatInputDisabled: boolean;
}

// SideDock is the run console's unified right-hand dock: Steering and Browser
// share one resizable column instead of stacking as two independent
// panels (which pushed the canvas into a cramped four-column row). Browser
// can join Steering as a second tab. Steering itself is permanent:
// it never floats or minimises, and stays mounted while Browser is selected
// so switching tabs preserves the transcript and composer state.
export function SideDock({
  runId,
  browserRightDocked,
  scrubSeq,
  browserDock,
  onBrowserDockChange,
  chatInputDisabled,
}: SideDockProps) {
  const [tab, setTab] = useState<"chat" | "browser">("chat");

  // Moving Browser back to the bottom always reveals permanent Steering.
  useEffect(() => {
    if (tab === "browser" && !browserRightDocked) {
      setTab("chat");
    }
  }, [tab, browserRightDocked]);

  const showChat = !browserRightDocked || tab === "chat";
  const showBrowser = browserRightDocked && tab === "browser";

  return (
    <div className="h-full border-l border-border-default min-h-0 overflow-hidden flex flex-col animate-fade-in-opacity">
      {browserRightDocked && (
        <Tabs
          value={tab}
          onValueChange={(v) => setTab(v as "chat" | "browser")}
          items={[
            { value: "chat", label: "Steering" },
            { value: "browser", label: "Browser" },
          ]}
          variant="underline"
          listClassName="px-1"
          className="border-b border-border-default shrink-0"
        />
      )}
      <div className={showChat ? "flex-1 min-h-0 flex flex-col" : "hidden"}>
        <SteeringPanel runId={runId} inputDisabled={chatInputDisabled} />
      </div>
      {browserRightDocked && (
        <div className={showBrowser ? "flex-1 min-h-0 flex flex-col" : "hidden"}>
          <BrowserPane
            runId={runId}
            scrubSeq={scrubSeq}
            dock={browserDock}
            onDockChange={onBrowserDockChange}
          />
        </div>
      )}
    </div>
  );
}
