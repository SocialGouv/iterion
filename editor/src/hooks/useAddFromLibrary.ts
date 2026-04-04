import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import {
  generateUniqueName,
  getAllNodeNames,
  getAllSchemaNames,
  getAllPromptNames,
  defaultAgent,
  defaultJudge,
  defaultRouter,
  defaultHuman,
  defaultTool,
} from "@/lib/defaults";
import type { LibraryItem } from "@/lib/library/types";
import type { IterDocument, SchemaDecl } from "@/api/types";

/** Check if two schemas have identical fields (for dedup, order-independent). */
function schemasEqual(a: SchemaDecl, b: SchemaDecl): boolean {
  if (a.fields.length !== b.fields.length) return false;
  const serialize = (f: SchemaDecl["fields"][number]) => JSON.stringify(f);
  const setA = new Set(a.fields.map(serialize));
  return b.fields.every((f) => setA.has(serialize(f)));
}

/** Generate a unique name for a schema/prompt, avoiding collisions. */
function uniqueName(base: string, existing: Set<string>): string {
  if (!existing.has(base)) return base;
  let i = 2;
  while (existing.has(`${base}_${i}`)) i++;
  return `${base}_${i}`;
}

/**
 * Hook that creates declarations from a library item as a single atomic operation.
 * Returns the new node name (or null for primitive-only items).
 */
export function useAddFromLibrary() {
  const document = useDocumentStore((s) => s.document);
  const applyBatch = useDocumentStore((s) => s.applyBatch);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

  const addFromLibrary = useCallback(
    (item: LibraryItem): string | null => {
      if (!document) return null;

      // Build name mappings for schemas and prompts (template name → actual name)
      const schemaNameMap = new Map<string, string>();
      const promptNameMap = new Map<string, string>();
      let createdNodeName: string | null = null;

      applyBatch((doc: IterDocument): IterDocument => {
        let result = { ...doc };

        // 1. Create schemas
        const templateSchemas = item.template.schemas ?? [];
        if (templateSchemas.length > 0) {
          const existingSchemas = getAllSchemaNames(result);
          const newSchemas = [...result.schemas];
          for (const tplSchema of templateSchemas) {
            const existingSchema = result.schemas.find((s) => s.name === tplSchema.name);
            if (existingSchema && schemasEqual(existingSchema, tplSchema)) {
              // Identical schema already exists — reuse
              schemaNameMap.set(tplSchema.name, tplSchema.name);
            } else {
              // Create with unique name
              const actualName = uniqueName(tplSchema.name, existingSchemas);
              existingSchemas.add(actualName);
              schemaNameMap.set(tplSchema.name, actualName);
              newSchemas.push({ ...tplSchema, name: actualName });
            }
          }
          result = { ...result, schemas: newSchemas };
        }

        // 2. Create prompts
        const templatePrompts = item.template.prompts ?? [];
        if (templatePrompts.length > 0) {
          const existingPrompts = getAllPromptNames(result);
          const newPrompts = [...result.prompts];
          for (const tplPrompt of templatePrompts) {
            const existingPrompt = result.prompts.find((p) => p.name === tplPrompt.name);
            if (existingPrompt && existingPrompt.body === tplPrompt.body) {
              promptNameMap.set(tplPrompt.name, tplPrompt.name);
            } else {
              const actualName = uniqueName(tplPrompt.name, existingPrompts);
              existingPrompts.add(actualName);
              promptNameMap.set(tplPrompt.name, actualName);
              newPrompts.push({ ...tplPrompt, name: actualName });
            }
          }
          result = { ...result, prompts: newPrompts };
        }

        // 3. Create vars
        const templateVars = item.template.vars ?? [];
        if (templateVars.length > 0) {
          const existingVarNames = new Set((result.vars?.fields ?? []).map((v) => v.name));
          const newFields = [...(result.vars?.fields ?? [])];
          for (const v of templateVars) {
            if (!existingVarNames.has(v.name)) {
              newFields.push({ ...v });
              existingVarNames.add(v.name);
            }
          }
          result = { ...result, vars: { fields: newFields } };
        }

        // 4. Create node (if this is a node template)
        const nodeTpl = item.template.node;
        if (nodeTpl) {
          const existingNames = getAllNodeNames(result);
          // Use item name as base (snake_case)
          const baseName = item.name.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_|_$/g, "");
          const nodeName = generateUniqueName(baseName, existingNames);

          const remapSchema = (ref: string | undefined): string => {
            if (!ref) return "";
            return schemaNameMap.get(ref) ?? ref;
          };
          const remapPrompt = (ref: string | undefined): string => {
            if (!ref) return "";
            return promptNameMap.get(ref) ?? ref;
          };

          createdNodeName = nodeName;

          switch (nodeTpl.kind) {
            case "agent": {
              const base = defaultAgent(nodeName);
              const data = nodeTpl.data;
              const schemas = item.template.schemas ?? [];
              const prompts = item.template.prompts ?? [];
              result = {
                ...result,
                agents: [
                  ...result.agents,
                  {
                    ...base,
                    ...data,
                    name: nodeName,
                    input: remapSchema(data.input ?? schemas[0]?.name),
                    output: remapSchema(data.output ?? schemas[1]?.name),
                    system: remapPrompt(data.system ?? prompts[0]?.name),
                    user: remapPrompt(data.user ?? prompts[1]?.name),
                  },
                ],
              };
              break;
            }
            case "judge": {
              const base = defaultJudge(nodeName);
              const data = nodeTpl.data;
              const schemas = item.template.schemas ?? [];
              const prompts = item.template.prompts ?? [];
              result = {
                ...result,
                judges: [
                  ...result.judges,
                  {
                    ...base,
                    ...data,
                    name: nodeName,
                    input: remapSchema(data.input ?? schemas[0]?.name),
                    output: remapSchema(data.output ?? schemas[1]?.name),
                    system: remapPrompt(data.system ?? prompts[0]?.name),
                    user: remapPrompt(data.user ?? prompts[1]?.name),
                  },
                ],
              };
              break;
            }
            case "router": {
              const base = defaultRouter(nodeName);
              result = {
                ...result,
                routers: [...result.routers, { ...base, ...nodeTpl.data, name: nodeName }],
              };
              break;
            }
            case "human": {
              const base = defaultHuman(nodeName);
              const data = nodeTpl.data;
              const schemas = item.template.schemas ?? [];
              const prompts = item.template.prompts ?? [];
              result = {
                ...result,
                humans: [
                  ...result.humans,
                  {
                    ...base,
                    ...data,
                    name: nodeName,
                    input: remapSchema(data.input ?? schemas[0]?.name),
                    output: remapSchema(data.output ?? schemas[1]?.name),
                    instructions: remapPrompt(data.instructions ?? prompts[0]?.name),
                  },
                ],
              };
              break;
            }
            case "tool": {
              const base = defaultTool(nodeName);
              const data = nodeTpl.data;
              const schemas = item.template.schemas ?? [];
              result = {
                ...result,
                tools: [
                  ...result.tools,
                  {
                    ...base,
                    ...data,
                    name: nodeName,
                    output: remapSchema(data.output ?? schemas[0]?.name),
                  },
                ],
              };
              break;
            }
          }

          return result;
        }

        return result;
      });

      if (createdNodeName) {
        setTimeout(() => setSelectedNode(createdNodeName!), 0);
      }

      return createdNodeName;
    },
    [document, applyBatch, setSelectedNode],
  );

  return addFromLibrary;
}
