import { useEffect, useState } from "react";

import { Tabs } from "@/components/ui";

import BrowserPane, { type BrowserDock } from "../BrowserPane";
import { ChatPanelContent } from "../FloatingChatPanel";

interface SideDockProps {
  runId: string;
  chatDockedRight: boolean;
  browserRightDocked: boolean;
  scrubSeq: number | null;
  browserDock: BrowserDock;
  onBrowserDockChange: (next: BrowserDock) => void;
  chatInputDisabled: boolean;
  onUndockChat: () => void;
  onCloseChat: () => void;
}

// SideDock is the run console's unified right-hand dock: Steering and Browser
// share one resizable column instead of stacking as two independent
// panels (which pushed the canvas into a cramped four-column row). When
// both are docked it shows a tab switcher; when only one is docked it
// renders that panel directly. Each panel keeps its own header/actions
// (undock/close for chat, move-to-bottom for browser). Both stay mounted
// while both are docked so switching tabs preserves scroll / session.
export function SideDock({
  runId,
  chatDockedRight,
  browserRightDocked,
  scrubSeq,
  browserDock,
  onBrowserDockChange,
  chatInputDisabled,
  onUndockChat,
  onCloseChat,
}: SideDockProps) {
  const bothDocked = chatDockedRight && browserRightDocked;
  const [tab, setTab] = useState<"chat" | "browser">(
    chatDockedRight ? "chat" : "browser",
  );

  // Keep the active tab pointing at a still-docked panel as docks toggle.
  useEffect(() => {
    if (tab === "chat" && !chatDockedRight && browserRightDocked) {
      setTab("browser");
    } else if (tab === "browser" && !browserRightDocked && chatDockedRight) {
      setTab("chat");
    }
  }, [tab, chatDockedRight, browserRightDocked]);

  const showChat = chatDockedRight && (!bothDocked || tab === "chat");
  const showBrowser = browserRightDocked && (!bothDocked || tab === "browser");

  return (
    <div className="h-full border-l border-border-default min-h-0 overflow-hidden flex flex-col animate-fade-in-opacity">
      {bothDocked && (
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
      {chatDockedRight && (
        <div className={showChat ? "flex-1 min-h-0 flex flex-col" : "hidden"}>
          <ChatPanelContent
            runId={runId}
            inputDisabled={chatInputDisabled}
            onUndock={onUndockChat}
            onClose={onCloseChat}
          />
        </div>
      )}
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
