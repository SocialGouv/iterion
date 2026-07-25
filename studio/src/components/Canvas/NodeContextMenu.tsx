import { useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
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

  // Auto-focus the first menuitem when the menu mounts (and any time the
  // visible item set changes, e.g. group-input panel closes) so keyboard
  // users land inside the menu with focus on something actionable.
  useEffect(() => {
    if (showGroupInput) return;
    const firstItem = ref.current?.querySelector<HTMLElement>('[role="menuitem"]:not([disabled])');
    firstItem?.focus();
  }, [showGroupInput]);

  const handleCreateGroup = () => {
    const name = groupName.trim();
    if (!name) return;
    onCreateGroup(name, selectedNodeIds);
    onClose();
  };

  /**
   * Roving focus between menu items via ArrowUp / ArrowDown. Wraps at
   * both ends so the menu feels complete without a tab trap.
   */
  const handleMenuKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const items = Array.from(
      ref.current?.querySelectorAll<HTMLElement>('[role="menuitem"]:not([disabled])') ?? [],
    );
    if (items.length === 0) return;
    const current = window.document.activeElement as HTMLElement | null;
    const idx = current ? items.indexOf(current) : -1;
    e.preventDefault();
    const next = e.key === "ArrowDown"
      ? items[(idx + 1) % items.length]
      : items[(idx - 1 + items.length) % items.length];
    next?.focus();
  };

  return (
    <div
      ref={ref}
      role="menu"
      aria-label="Node actions"
      onKeyDown={handleMenuKeyDown}
      className="fixed bg-surface-1 border border-border-strong rounded-lg shadow-[var(--shadow-popover)] z-[var(--z-popover)] py-1 min-w-[160px]"
      style={{
        left: Math.min(x, window.innerWidth - 180),
        top: Math.min(y, window.innerHeight - 200),
      }}
    >
      <div className="px-3 py-1 text-caption text-fg-subtle uppercase tracking-wider">
        {isGroupNode ? groupNameFromNodeId(nodeId) : nodeId}
      </div>

      {/* Group node actions */}
      {isGroupNode && (
        <>
          <button
            type="button"
            role="menuitem"
            className="w-full text-left px-3 py-1.5 hover:bg-danger-soft text-xs text-danger flex items-center gap-2 focus-visible:outline-none focus-visible:bg-danger-soft"
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
            type="button"
            role="menuitem"
            className="w-full text-left px-3 py-1.5 hover:bg-surface-2 text-xs text-fg-default flex items-center gap-2 focus-visible:outline-none focus-visible:bg-surface-2 disabled:opacity-50"
            onClick={() => { onSetEntry(); onClose(); }}
            disabled={isEntry}
          >
            <span className="text-warning-fg">&#x25B6;</span>
            {isEntry ? "Already entry point" : "Set as entry point"}
          </button>
          <button
            type="button"
            role="menuitem"
            className="w-full text-left px-3 py-1.5 hover:bg-surface-2 text-xs text-fg-default flex items-center gap-2 focus-visible:outline-none focus-visible:bg-surface-2"
            onClick={() => { onDuplicate(); onClose(); }}
          >
            <span className="text-accent-text">&#x2398;</span>
            Duplicate
          </button>

          {/* Group operations */}
          <div className="border-t border-border-default my-1" />

          {canGroup && !showGroupInput && (
            <button
              type="button"
              role="menuitem"
              className="w-full text-left px-3 py-1.5 hover:bg-surface-2 text-xs text-fg-default flex items-center gap-2 focus-visible:outline-none focus-visible:bg-surface-2"
              onClick={() => setShowGroupInput(true)}
            >
              <span className="text-accent-text">{"\u{1F4E6}"}</span>
              Group {selectedNodeIds.length} nodes...
            </button>
          )}

          {showGroupInput && (
            <div className="px-3 py-1.5 flex gap-1">
              <input
                ref={inputRef}
                aria-label="Group name"
                className="flex-1 bg-surface-0 border border-border-strong rounded px-2 py-1 text-xs text-fg-default placeholder:text-fg-subtle"
                placeholder="Group name..."
                value={groupName}
                onChange={(e) => setGroupName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") handleCreateGroup();
                  e.stopPropagation();
                }}
              />
              <button
                type="button"
                className="bg-accent hover:bg-accent-hover text-fg-onAccent text-xs px-2 py-1 rounded shrink-0"
                onClick={handleCreateGroup}
              >
                OK
              </button>
            </div>
          )}

          {belongsToGroup && (
            <button
              type="button"
              role="menuitem"
              className="w-full text-left px-3 py-1.5 hover:bg-surface-2 text-xs text-fg-muted flex items-center gap-2 focus-visible:outline-none focus-visible:bg-surface-2"
              onClick={() => { onRemoveFromGroup(belongsToGroup, nodeId); onClose(); }}
            >
              <span className="text-fg-subtle">{"\u{1F4E4}"}</span>
              Remove from "{belongsToGroup}"
            </button>
          )}

          <div className="border-t border-border-default my-1" />
          <button
            type="button"
            role="menuitem"
            className="w-full text-left px-3 py-1.5 hover:bg-danger-soft text-xs text-danger flex items-center gap-2 focus-visible:outline-none focus-visible:bg-danger-soft"
            onClick={() => { onDelete(); onClose(); }}
          >
            <span>&#x2716;</span>
            Delete
          </button>
        </>
      )}
      {isTerminal && (
        <div className="px-3 py-1.5 text-xs text-fg-subtle">
          Terminal node (no actions)
        </div>
      )}
    </div>
  );
}
