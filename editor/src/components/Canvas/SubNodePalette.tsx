import { type DragEvent, useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { findNodeDecl } from "@/lib/defaults";
import { SUB_COLORS, SUB_ICONS } from "@/lib/constants";
import type { SubNodeRelation } from "@/lib/docMutations";
import type { NodeKind, AgentDecl, JudgeDecl, HumanDecl } from "@/api/types";
import type { SubNodeDragData } from "@/hooks/useAddSubNode";

interface PaletteItem {
  subKind: SubNodeDragData["subKind"];
  relation?: SubNodeRelation;
  label: string;
}

function getNewItemsForKind(kind: NodeKind): PaletteItem[] {
  switch (kind) {
    case "agent":
    case "judge":
      return [
        { subKind: "schema", relation: "input", label: "Input Schema" },
        { subKind: "schema", relation: "output", label: "Output Schema" },
        { subKind: "prompt", relation: "system", label: "System Prompt" },
        { subKind: "prompt", relation: "user", label: "User Prompt" },
        { subKind: "var", label: "Variable" },
      ];
    case "router":
      return [
        { subKind: "prompt", relation: "system", label: "System Prompt" },
        { subKind: "prompt", relation: "user", label: "User Prompt" },
      ];
    case "human":
      return [
        { subKind: "schema", relation: "input", label: "Input Schema" },
        { subKind: "schema", relation: "output", label: "Output Schema" },
        { subKind: "prompt", relation: "instructions", label: "Instructions" },
        { subKind: "var", label: "Variable" },
      ];
    case "tool":
      return [
        { subKind: "schema", relation: "output", label: "Output Schema" },
      ];
    default:
      return [];
  }
}

function onDragStart(e: DragEvent, data: SubNodeDragData) {
  e.dataTransfer.setData("application/iterion-subnode", JSON.stringify(data));
  e.dataTransfer.effectAllowed = "move";
}

function DraggableItem({ item, dragData }: { item: { subKind: SubNodeDragData["subKind"]; label: string }; dragData: SubNodeDragData }) {
  const color = SUB_COLORS[item.subKind];
  const icon = SUB_ICONS[item.subKind];
  return (
    <div
      draggable
      onDragStart={(e) => onDragStart(e, dragData)}
      className="flex items-center gap-2 px-2 py-1.5 rounded cursor-grab hover:brightness-125 transition-all border border-gray-600"
      style={{ backgroundColor: color + "18", borderColor: color + "66" }}
      title={`Drag to add ${item.label}`}
    >
      <span className="text-xs">{icon}</span>
      <span className="text-[10px] text-gray-200 truncate">{item.label}</span>
    </div>
  );
}

export default function SubNodePalette() {
  const document = useDocumentStore((s) => s.document);
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const centralNodeId = subNodeViewStack.length > 0 ? subNodeViewStack[subNodeViewStack.length - 1]! : null;

  const found = useMemo(() => {
    if (!document || !centralNodeId) return null;
    return findNodeDecl(document, centralNodeId);
  }, [document, centralNodeId]);

  const newItems = useMemo(() => {
    if (!found) return [];
    return getNewItemsForKind(found.kind);
  }, [found]);

  // Compute unlinked existing items
  const unlinked = useMemo(() => {
    if (!document || !found || !centralNodeId) return { schemas: [] as string[], prompts: [] as string[], tools: [] as string[] };

    const decl = found.decl;
    const kind = found.kind;

    // Schemas linked to this node
    const linkedSchemas = new Set<string>();
    if ("input" in decl && decl.input) linkedSchemas.add(decl.input as string);
    if ("output" in decl && decl.output) linkedSchemas.add(decl.output as string);
    const unlinkedSchemas = (document.schemas ?? [])
      .map((s) => s.name)
      .filter((n) => !linkedSchemas.has(n));

    // Prompts linked to this node
    const linkedPrompts = new Set<string>();
    if ("system" in decl && decl.system) linkedPrompts.add(decl.system as string);
    if ("user" in decl && decl.user) linkedPrompts.add(decl.user as string);
    if ("instructions" in decl && (decl as HumanDecl).instructions) linkedPrompts.add((decl as HumanDecl).instructions);
    const unlinkedPrompts = (document.prompts ?? [])
      .map((p) => p.name)
      .filter((n) => !linkedPrompts.has(n));

    // Tools not yet assigned to this agent/judge
    const unlinkedTools: string[] = [];
    if (kind === "agent" || kind === "judge") {
      const assignedTools = new Set((decl as AgentDecl | JudgeDecl).tools ?? []);
      for (const t of document.tools) {
        if (!assignedTools.has(t.name)) unlinkedTools.push(t.name);
      }
    }

    return { schemas: unlinkedSchemas, prompts: unlinkedPrompts, tools: unlinkedTools };
  }, [document, found, centralNodeId]);

  if (!found || !centralNodeId) return null;

  const hasUnlinked = unlinked.schemas.length > 0 || unlinked.prompts.length > 0 || unlinked.tools.length > 0;

  return (
    <div className="flex flex-col h-full">
      {/* Header */}
      <div className="px-3 py-2 border-b border-gray-700">
        <span className="text-xs font-semibold text-gray-300 uppercase tracking-wider">Sub-nodes</span>
        <div className="text-[9px] text-gray-500 mt-0.5 truncate">{centralNodeId}</div>
      </div>

      {/* Create new section */}
      <div className="px-2 py-2">
        <span className="text-[9px] text-gray-500 uppercase tracking-wider px-1">Create New</span>
        <div className="flex flex-col gap-1 mt-1">
          {newItems.map((item) => (
            <DraggableItem
              key={`${item.subKind}-${item.relation ?? ""}`}
              item={item}
              dragData={{ subKind: item.subKind, relation: item.relation }}
            />
          ))}
        </div>
      </div>

      {/* Existing unlinked section */}
      {hasUnlinked && (
        <>
          <div className="border-t border-gray-700 mx-2" />
          <div className="px-2 py-2 flex-1 overflow-y-auto">
            <span className="text-[9px] text-gray-500 uppercase tracking-wider px-1">Existing</span>
            <div className="flex flex-col gap-1 mt-1">
              {unlinked.schemas.map((name) => (
                <DraggableItem
                  key={`schema-${name}`}
                  item={{ subKind: "schema", label: name }}
                  dragData={{ subKind: "schema", existingName: name }}
                />
              ))}
              {unlinked.prompts.map((name) => (
                <DraggableItem
                  key={`prompt-${name}`}
                  item={{ subKind: "prompt", label: name }}
                  dragData={{ subKind: "prompt", existingName: name }}
                />
              ))}
              {unlinked.tools.map((name) => (
                <DraggableItem
                  key={`tool-${name}`}
                  item={{ subKind: "tool", label: name }}
                  dragData={{ subKind: "tool", existingName: name }}
                />
              ))}
            </div>
          </div>
        </>
      )}
    </div>
  );
}
