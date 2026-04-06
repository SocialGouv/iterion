import { useEffect, useRef, useState } from "react";
import { isGroupNodeId, groupNameFromNodeId } from "@/lib/groups";

interface Props {
  x: number;
  y: number;
  nodeId: string;
  isTerminal: boolean;
  isEntry: boolean;
  /** IDs of all currently selected nodes (ReactFlow multi-select). */
  selectedNodeIds: string[];
  /** Name of the group this node belongs to (if any). */
  belongsToGroup: string | null;
  onSetEntry: () => void;
  onDuplicate: () => void;
  onDelete: () => void;
  onClose: () => void;
  onCreateGroup: (name: string, nodeIds: string[]) => void;
  onRemoveGroup: (groupName: string) => void;
  onRemoveFromGroup: (groupName: string, nodeId: string) => void;
}

export default function NodeContextMenu({
  x,
  y,
  nodeId,
  isTerminal,
  isEntry,
  selectedNodeIds,
  belongsToGroup,
  onSetEntry,
  onDuplicate,
  onDelete,
  onClose,
  onCreateGroup,
  onRemoveGroup,
  onRemoveFromGroup,
}: Props) {
  const ref = useRef<HTMLDivElement>(null);
  const [showGroupInput, setShowGroupInput] = useState(false);
  const [groupName, setGroupName] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  const isGroupNode = isGroupNodeId(nodeId);
  const canGroup = selectedNodeIds.length >= 2;

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        onClose();
      }
    };
    const keyHandler = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        if (showGroupInput) setShowGroupInput(false);
        else onClose();
      }
    };
    window.document.addEventListener("mousedown", handler);
    window.addEventListener("keydown", keyHandler);
    return () => {
      window.document.removeEventListener("mousedown", handler);
      window.removeEventListener("keydown", keyHandler);
    };
  }, [onClose, showGroupInput]);

  useEffect(() => {
    if (showGroupInput && inputRef.current) inputRef.current.focus();
  }, [showGroupInput]);

  const handleCreateGroup = () => {
    const name = groupName.trim();
    if (!name) return;
    onCreateGroup(name, selectedNodeIds);
    onClose();
  };

  return (
    <div
      ref={ref}
      className="fixed bg-gray-800 border border-gray-600 rounded-lg shadow-xl z-50 py-1 min-w-[160px]"
      style={{
        left: Math.min(x, window.innerWidth - 180),
        top: Math.min(y, window.innerHeight - 200),
      }}
    >
      <div className="px-3 py-1 text-[10px] text-gray-500 uppercase tracking-wider">
        {isGroupNode ? groupNameFromNodeId(nodeId) : nodeId}
      </div>

      {/* Group node actions */}
      {isGroupNode && (
        <>
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-red-900/50 text-xs text-red-400 flex items-center gap-2"
            onClick={() => { onRemoveGroup(groupNameFromNodeId(nodeId)); onClose(); }}
          >
            <span>{"\u{1F4E4}"}</span>
            Ungroup
          </button>
        </>
      )}

      {/* Regular node actions */}
      {!isTerminal && !isGroupNode && (
        <>
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
            onClick={() => { onSetEntry(); onClose(); }}
            disabled={isEntry}
          >
            <span className="text-amber-400">&#x25B6;</span>
            {isEntry ? "Already entry point" : "Set as entry point"}
          </button>
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
            onClick={() => { onDuplicate(); onClose(); }}
          >
            <span className="text-blue-400">&#x2398;</span>
            Duplicate
          </button>

          {/* Group operations */}
          <div className="border-t border-gray-700 my-1" />

          {canGroup && !showGroupInput && (
            <button
              className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
              onClick={() => setShowGroupInput(true)}
            >
              <span className="text-indigo-400">{"\u{1F4E6}"}</span>
              Group {selectedNodeIds.length} nodes...
            </button>
          )}

          {showGroupInput && (
            <div className="px-3 py-1.5">
              <input
                ref={inputRef}
                className="w-full bg-gray-900 border border-gray-600 rounded px-2 py-1 text-xs text-white placeholder-gray-500"
                placeholder="Group name..."
                value={groupName}
                onChange={(e) => setGroupName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") handleCreateGroup();
                  e.stopPropagation();
                }}
              />
            </div>
          )}

          {belongsToGroup && (
            <button
              className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-gray-300 flex items-center gap-2"
              onClick={() => { onRemoveFromGroup(belongsToGroup, nodeId); onClose(); }}
            >
              <span className="text-gray-500">{"\u{1F4E4}"}</span>
              Remove from "{belongsToGroup}"
            </button>
          )}

          <div className="border-t border-gray-700 my-1" />
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-red-900/50 text-xs text-red-400 flex items-center gap-2"
            onClick={() => { onDelete(); onClose(); }}
          >
            <span>&#x2716;</span>
            Delete
          </button>
        </>
      )}
      {isTerminal && (
        <div className="px-3 py-1.5 text-xs text-gray-500">
          Terminal node (no actions)
        </div>
      )}
    </div>
  );
}
