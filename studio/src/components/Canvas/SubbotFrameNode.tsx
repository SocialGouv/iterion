import { Handle } from "@xyflow/react";
import type { Node, NodeProps } from "@xyflow/react";
import { useLocation } from "wouter";
import { ExternalLink } from "lucide-react";
import { useTabsStore } from "@/store/tabs";
import { SELECTED_BORDER, SELECTED_GLOW, softColor } from "@/lib/constants";
import type { SubbotFrameData } from "@/lib/subbotGraph";
import { NodeIcon } from "@/components/icons/NodeIcon";
import { SIDES, POS_MAP } from "./handlePositions";

// Container node framing an expanded subbot's child workflow on the EDITOR
// canvas. Unlike the run console's FanoutFrame (a background-only bbox) this
// is a real React Flow parent: the child bot's nodes carry parentId pointing
// here and ELK sizes it as a compound. The SOLID border (groups/fanout are
// dashed) + the source-file header make it obvious the content is another
// bot's graph, not part of the edited file.

const SUBBOT_COLOR = "var(--color-node-subbot)";

type SubbotFrameType = Node<SubbotFrameData, "subbotFrame">;

export default function SubbotFrameNode({ data, selected }: NodeProps<SubbotFrameType>) {
  const { label, source, sourcePath, isolated } = data;
  const [, setLocation] = useLocation();

  const openChild = () => {
    if (!sourcePath) return;
    // Same flow as RecentFilesPanel: openTab creates/focuses the editor tab,
    // the URL keeps deep-link + strip state in sync.
    useTabsStore.getState().openTab("editor", { file: sourcePath });
    setLocation(`/editor?file=${encodeURIComponent(sourcePath)}`);
  };

  return (
    <div
      className="rounded-xl border-2 w-full h-full"
      style={{
        borderColor: selected ? SELECTED_BORDER : softColor(SUBBOT_COLOR, 55),
        borderStyle: "solid",
        background: softColor(SUBBOT_COLOR, 5),
        boxShadow: selected ? SELECTED_GLOW : undefined,
        minWidth: "100%",
        minHeight: "100%",
      }}
    >
      {/* Invisible handles: parent edges normally attach to the child's
          entry/done nodes, but fall back to the frame itself when the child
          graph has no matching node (e.g. no done terminal). */}
      {SIDES.map((s) => (
        <Handle key={`target-${s}`} id={`target-${s}`} type="target" position={POS_MAP[s]} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
      ))}
      <div
        className="flex items-center gap-1.5 px-3 py-2 select-none"
        style={{ borderBottom: `1px solid ${softColor(SUBBOT_COLOR, 25)}` }}
        title={`Subbot ${label}: runs ${source} as a separate child run. Its nodes are read-only here — open the file to edit.`}
      >
        <NodeIcon kind="subbot" size={14} />
        <span className="text-xs font-semibold text-fg-default">{label}</span>
        <span className="text-[9px] uppercase tracking-wider text-fg-subtle">· subbot</span>
        <span className="font-mono text-caption text-fg-muted truncate">{source}</span>
        {isolated && (
          <span
            className="text-[9px] px-1 rounded"
            style={{ background: softColor(SUBBOT_COLOR, 18), color: SUBBOT_COLOR }}
            title="isolated: each child run confines its writes to its own run store"
          >
            isolated
          </span>
        )}
        <button
          type="button"
          className="ml-auto shrink-0 inline-flex items-center justify-center w-5 h-5 rounded text-fg-subtle hover:text-fg-default hover:bg-surface-2"
          title={`Open ${source} in a new editor tab`}
          aria-label={`Open ${source} in a new editor tab`}
          onClick={(e) => {
            e.stopPropagation();
            openChild();
          }}
        >
          <ExternalLink size={12} aria-hidden />
        </button>
      </div>
      {SIDES.map((s) => (
        <Handle key={`source-${s}`} id={`source-${s}`} type="source" position={POS_MAP[s]} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
      ))}
    </div>
  );
}
