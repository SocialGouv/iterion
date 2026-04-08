import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { findNodeDecl, defaultSchema, defaultPrompt, generateUniqueName, getAllSchemaNames, getAllPromptNames } from "@/lib/defaults";
import { assignFieldToNode, addToolToNode } from "@/lib/docMutations";
import type { SubNodeRelation } from "@/lib/docMutations";
import { DETAIL_PREFIX_SCHEMA, DETAIL_PREFIX_PROMPT, DETAIL_PREFIX_VAR, DETAIL_PREFIX_TOOL } from "@/lib/nodeDetailGraph";
import type { VarsBlock } from "@/api/types";

export interface SubNodeDragData {
  subKind: "schema" | "prompt" | "var" | "tool";
  relation?: SubNodeRelation;
  existingName?: string;
}

/**
 * Hook to create/assign subnodes in the detail view.
 * Returns the predicted detail node ID for positioning, or null on failure.
 */
export function useAddSubNode() {
  const document = useDocumentStore((s) => s.document);
  const applyBatch = useDocumentStore((s) => s.applyBatch);
  const addToast = useUIStore((s) => s.addToast);

  return useCallback(
    (data: SubNodeDragData, centralNodeId: string): string | null => {
      if (!document) return null;

      const found = findNodeDecl(document, centralNodeId);
      if (!found) return null;

      const { subKind, relation, existingName } = data;

      if (subKind === "schema") {
        if (!relation) return null;
        const schemaName = existingName ?? generateUniqueName("schema", getAllSchemaNames(document));
        applyBatch((doc) => {
          let result = doc;
          if (!existingName) {
            result = { ...result, schemas: [...result.schemas, defaultSchema(schemaName)] };
          }
          return assignFieldToNode(result, centralNodeId, relation, schemaName);
        });
        addToast(`Schema "${schemaName}" assigned as ${relation}`, "success");
        return DETAIL_PREFIX_SCHEMA + schemaName + ":" + relation;
      }

      if (subKind === "prompt") {
        if (!relation) return null;
        const promptName = existingName ?? generateUniqueName("prompt", getAllPromptNames(document));
        applyBatch((doc) => {
          let result = doc;
          if (!existingName) {
            result = { ...result, prompts: [...result.prompts, defaultPrompt(promptName)] };
          }
          return assignFieldToNode(result, centralNodeId, relation, promptName);
        });
        addToast(`Prompt "${promptName}" assigned as ${relation}`, "success");
        return DETAIL_PREFIX_PROMPT + promptName + ":" + relation;
      }

      if (subKind === "var") {
        const existingVarNames = new Set((document.vars?.fields ?? []).map((v) => v.name));
        const varName = existingName ?? generateUniqueName("var", existingVarNames);

        if (existingName) {
          addToast(`Reference variable with {{vars.${varName}}} in a prompt`, "info");
          return DETAIL_PREFIX_VAR + varName;
        }

        applyBatch((doc) => {
          const currentFields = doc.vars?.fields ?? [];
          const newVars: VarsBlock = { fields: [...currentFields, { name: varName, type: "string" }] };
          return { ...doc, vars: newVars };
        });
        addToast(`Variable "${varName}" created. Use {{vars.${varName}}} in a prompt`, "info");
        return DETAIL_PREFIX_VAR + varName;
      }

      if (subKind === "tool") {
        if (!existingName) return null;
        const { kind } = found;
        if (kind !== "agent" && kind !== "judge") {
          addToast("Only agents and judges can have tools", "error");
          return null;
        }
        applyBatch((doc) => addToolToNode(doc, centralNodeId, existingName));
        addToast(`Tool "${existingName}" added`, "success");
        return DETAIL_PREFIX_TOOL + existingName;
      }

      return null;
    },
    [document, applyBatch, addToast],
  );
}
