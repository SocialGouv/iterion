import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
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
  defaultCompute,
} from "@/lib/defaults";
import { parseGroups, groupToCommentText } from "@/lib/groups";
import type { LibraryItem, NodeTemplate } from "@/lib/library/types";
import type { IterDocument, SchemaDecl, VarField } from "@/api/types";

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

/** Convert a display name to a snake_case identifier. */
function toSnakeCase(text: string): string {
  return text.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_|_$/g, "");
}

/** Terminal node names that should not be remapped in edge templates. */
const TERMINAL_NAMES = new Set(["done", "fail", "__start__"]);

/** Create a single node declaration in the document, returning the mutated result. */
function createNodeInResult(
  result: IterDocument,
  nodeTpl: NodeTemplate,
  nodeName: string,
  schemaNameMap: Map<string, string>,
  promptNameMap: Map<string, string>,
  templateSchemas: SchemaDecl[],
  templatePrompts: { name: string; body: string }[],
): IterDocument {
  const remapSchema = (ref: string | undefined): string => {
    if (!ref) return "";
    return schemaNameMap.get(ref) ?? ref;
  };
  const remapPrompt = (ref: string | undefined): string => {
    if (!ref) return "";
    return promptNameMap.get(ref) ?? ref;
  };

  switch (nodeTpl.kind) {
    case "agent": {
      const base = defaultAgent(nodeName);
      const data = nodeTpl.data;
      return {
        ...result,
        agents: [
          ...result.agents,
          {
            ...base,
            ...data,
            name: nodeName,
            input: remapSchema(data.input ?? templateSchemas[0]?.name),
            output: remapSchema(data.output ?? templateSchemas[1]?.name),
            system: remapPrompt(data.system ?? templatePrompts[0]?.name),
            user: remapPrompt(data.user ?? templatePrompts[1]?.name),
          },
        ],
      };
    }
    case "judge": {
      const base = defaultJudge(nodeName);
      const data = nodeTpl.data;
      return {
        ...result,
        judges: [
          ...result.judges,
          {
            ...base,
            ...data,
            name: nodeName,
            input: remapSchema(data.input ?? templateSchemas[0]?.name),
            output: remapSchema(data.output ?? templateSchemas[1]?.name),
            system: remapPrompt(data.system ?? templatePrompts[0]?.name),
            user: remapPrompt(data.user ?? templatePrompts[1]?.name),
          },
        ],
      };
    }
    case "router": {
      const base = defaultRouter(nodeName);
      return {
        ...result,
        routers: [...result.routers, { ...base, ...nodeTpl.data, name: nodeName }],
      };
    }
    case "human": {
      const base = defaultHuman(nodeName);
      const data = nodeTpl.data;
      return {
        ...result,
        humans: [
          ...result.humans,
          {
            ...base,
            ...data,
            name: nodeName,
            input: remapSchema(data.input ?? templateSchemas[0]?.name),
            output: remapSchema(data.output ?? templateSchemas[1]?.name),
            instructions: remapPrompt(data.instructions ?? templatePrompts[0]?.name),
          },
        ],
      };
    }
    case "tool": {
      const base = defaultTool(nodeName);
      const data = nodeTpl.data;
      return {
        ...result,
        tools: [
          ...result.tools,
          {
            ...base,
            ...data,
            name: nodeName,
            output: remapSchema(data.output ?? templateSchemas[0]?.name),
          },
        ],
      };
    }
    case "compute": {
      const base = defaultCompute(nodeName);
      const data = nodeTpl.data;
      return {
        ...result,
        computes: [
          ...result.computes,
          {
            ...base,
            ...data,
            name: nodeName,
            input:
              data.input !== undefined
                ? remapSchema(data.input) || undefined
                : undefined,
            output: remapSchema(data.output ?? templateSchemas[0]?.name ?? ""),
            expr: data.expr ?? [],
          },
        ],
      };
    }
  }
}

/** Process schemas from a template entry, deduplicating against existing ones. */
function processSchemas(
  result: IterDocument,
  templateSchemas: SchemaDecl[],
  schemaNameMap: Map<string, string>,
): IterDocument {
  if (templateSchemas.length === 0) return result;
  const existingSchemas = getAllSchemaNames(result);
  const newSchemas = [...result.schemas];
  for (const tplSchema of templateSchemas) {
    const existingSchema = result.schemas.find((s) => s.name === tplSchema.name);
    if (existingSchema && schemasEqual(existingSchema, tplSchema)) {
      schemaNameMap.set(tplSchema.name, tplSchema.name);
    } else {
      const actualName = uniqueName(tplSchema.name, existingSchemas);
      existingSchemas.add(actualName);
      schemaNameMap.set(tplSchema.name, actualName);
      newSchemas.push({ ...tplSchema, name: actualName });
    }
  }
  return { ...result, schemas: newSchemas };
}

/** Process prompts from a template entry, deduplicating against existing ones. */
function processPrompts(
  result: IterDocument,
  templatePrompts: { name: string; body: string }[],
  promptNameMap: Map<string, string>,
): IterDocument {
  if (templatePrompts.length === 0) return result;
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
  return { ...result, prompts: newPrompts };
}

/** Process vars from a template entry, merging into existing vars. */
function processVars(
  result: IterDocument,
  templateVars: VarField[],
): IterDocument {
  if (templateVars.length === 0) return result;
  const existingVarNames = new Set((result.vars?.fields ?? []).map((v) => v.name));
  const newFields = [...(result.vars?.fields ?? [])];
  for (const v of templateVars) {
    if (!existingVarNames.has(v.name)) {
      newFields.push({ ...v });
      existingVarNames.add(v.name);
    }
  }
  return { ...result, vars: { fields: newFields } };
}

/**
 * Hook that creates declarations from a library item as a single atomic operation.
 * Returns the new node name (single item), array of names (pattern), or null (primitive-only).
 */
export function useAddFromLibrary() {
  const document = useDocumentStore((s) => s.document);
  const applyBatch = useDocumentStore((s) => s.applyBatch);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const activeWorkflowName = useUIStore((s) => s.activeWorkflowName);

  const addFromLibrary = useCallback(
    (item: LibraryItem): string | string[] | null => {
      if (!document) return null;

      // ── Pattern path: multi-node with edges and group ──
      if (item.template.pattern) {
        const pattern = item.template.pattern;
        const createdNames: string[] = [];
        const placeholderToName = new Map<string, string>();

        applyBatch((doc: IterDocument): IterDocument => {
          let result = { ...doc };
          const existingNames = getAllNodeNames(result);
          const schemaNameMap = new Map<string, string>();
          const promptNameMap = new Map<string, string>();

          // 1. Create all nodes with their schemas/prompts
          for (const entry of pattern.nodes) {
            const entrySchemas = entry.schemas ?? [];
            const entryPrompts = entry.prompts ?? [];
            const entryVars = entry.vars ?? [];

            result = processSchemas(result, entrySchemas, schemaNameMap);
            result = processPrompts(result, entryPrompts, promptNameMap);
            result = processVars(result, entryVars);

            const nodeName = generateUniqueName(entry.placeholder, existingNames);
            existingNames.add(nodeName);
            placeholderToName.set(entry.placeholder, nodeName);
            createdNames.push(nodeName);

            result = createNodeInResult(
              result, entry.node, nodeName,
              schemaNameMap, promptNameMap,
              entrySchemas, entryPrompts,
            );
          }

          // 2. Create edges in the active workflow
          if (pattern.edges.length > 0) {
            const wfs = result.workflows ?? [];
            const wfName = activeWorkflowName ?? wfs[0]?.name;
            if (wfName) {
              const remap = (name: string) =>
                TERMINAL_NAMES.has(name) ? name : (placeholderToName.get(name) ?? name);

              result = {
                ...result,
                workflows: result.workflows.map((w) => {
                  if (w.name !== wfName) return w;
                  const newEdges = pattern.edges.map((e) => ({
                    from: remap(e.from),
                    to: remap(e.to),
                    ...(e.when ? { when: e.when } : {}),
                    ...(e.loop ? { loop: e.loop } : {}),
                    ...(e.with ? { with: e.with } : {}),
                  }));
                  return { ...w, edges: [...w.edges, ...newEdges] };
                }),
              };
            }
          }

          // 3. Create the group annotation
          const baseGroupName = pattern.groupName ?? toSnakeCase(item.name);
          const existingGroups = parseGroups(result.comments);
          const existingGroupNames = new Set(existingGroups.map((g) => g.name));
          const groupName = uniqueName(baseGroupName, existingGroupNames);
          const comment = { text: groupToCommentText({ name: groupName, nodeIds: createdNames }) };
          result = { ...result, comments: [...result.comments, comment] };

          return result;
        });

        if (createdNames.length > 0) {
          setTimeout(() => setSelectedNode(createdNames[0]!), 0);
        }
        return createdNames.length > 0 ? createdNames : null;
      }

      // ── Single-node path (unchanged logic) ──
      const schemaNameMap = new Map<string, string>();
      const promptNameMap = new Map<string, string>();
      let createdNodeName: string | null = null;

      applyBatch((doc: IterDocument): IterDocument => {
        let result = { ...doc };

        result = processSchemas(result, item.template.schemas ?? [], schemaNameMap);
        result = processPrompts(result, item.template.prompts ?? [], promptNameMap);
        result = processVars(result, item.template.vars ?? []);

        const nodeTpl = item.template.node;
        if (nodeTpl) {
          const existingNames = getAllNodeNames(result);
          const baseName = toSnakeCase(item.name);
          const nodeName = generateUniqueName(baseName, existingNames);
          createdNodeName = nodeName;

          result = createNodeInResult(
            result, nodeTpl, nodeName,
            schemaNameMap, promptNameMap,
            item.template.schemas ?? [], item.template.prompts ?? [],
          );
        }

        return result;
      });

      if (createdNodeName) {
        setTimeout(() => setSelectedNode(createdNodeName!), 0);
      }

      return createdNodeName;
    },
    [document, applyBatch, setSelectedNode, activeWorkflowName],
  );

  return addFromLibrary;
}
