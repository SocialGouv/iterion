import { useCallback, useMemo, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import type { Edge, WhenClause, LoopClause, WithEntry } from "@/api/types";
import { TextField, NumberField, CheckboxField, SelectField, CommittedTextField } from "./FormField";

interface Props {
  edge: Edge;
  edgeIndex: number;
  workflowName: string;
}

export default function EdgeForm({ edge, edgeIndex, workflowName }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateEdge = useDocumentStore((s) => s.updateEdge);
  const removeEdge = useDocumentStore((s) => s.removeEdge);

  const setWhen = useCallback(
    (when: WhenClause | undefined) => updateEdge(workflowName, edgeIndex, { when }),
    [workflowName, edgeIndex, updateEdge],
  );

  const setLoop = useCallback(
    (loop: LoopClause | undefined) => updateEdge(workflowName, edgeIndex, { loop }),
    [workflowName, edgeIndex, updateEdge],
  );

  const setWith = useCallback(
    (withEntries: WithEntry[] | undefined) => updateEdge(workflowName, edgeIndex, { with: withEntries }),
    [workflowName, edgeIndex, updateEdge],
  );

  // Get boolean fields from source node's output schema for condition suggestions
  const boolFieldOptions = useMemo(() => {
    if (!document) return [];
    const sourceNode = edge.from;
    // Find the source node's output schema
    let outputSchemaName = "";
    for (const a of document.agents) { if (a.name === sourceNode) { outputSchemaName = a.output; break; } }
    if (!outputSchemaName) for (const j of document.judges) { if (j.name === sourceNode) { outputSchemaName = j.output; break; } }
    if (!outputSchemaName) for (const h of document.humans) { if (h.name === sourceNode) { outputSchemaName = h.output; break; } }
    if (!outputSchemaName) for (const t of document.tools) { if (t.name === sourceNode) { outputSchemaName = t.output; break; } }
    if (!outputSchemaName) return [];
    // Find the schema and filter bool fields
    const schema = document.schemas.find((s) => s.name === outputSchemaName);
    if (!schema) return [];
    return schema.fields
      .filter((f) => f.type === "bool")
      .map((f) => ({ value: f.name, label: f.name }));
  }, [document, edge.from]);

  const activeWorkflow = useActiveWorkflow();

  // Build enum hints for target node's input schema fields
  const targetEnumMap = useMemo(() => {
    if (!document) return new Map<string, string[]>();
    const map = new Map<string, string[]>();
    const targetNode = edge.to;
    let inputSchemaName = "";
    for (const a of document.agents) { if (a.name === targetNode) { inputSchemaName = a.input; break; } }
    if (!inputSchemaName) for (const j of document.judges) { if (j.name === targetNode) { inputSchemaName = j.input; break; } }
    if (!inputSchemaName) for (const h of document.humans) { if (h.name === targetNode) { inputSchemaName = h.input; break; } }
    if (inputSchemaName) {
      const schema = document.schemas.find((s) => s.name === inputSchemaName);
      if (schema) {
        for (const f of schema.fields) {
          if (f.enum_values && f.enum_values.length > 0) {
            map.set(f.name, f.enum_values);
          }
        }
      }
    }
    return map;
  }, [document, edge.to]);

  // Build template reference suggestions for With value fields
  const templateRefs = useMemo(() => {
    if (!document) return [];
    const refs: { label: string; value: string; group: string }[] = [];

    // {{input.*}} from target node's input schema
    const targetNode = edge.to;
    let targetInputSchemaName = "";
    for (const a of document.agents) { if (a.name === targetNode) { targetInputSchemaName = a.input; break; } }
    if (!targetInputSchemaName) for (const j of document.judges) { if (j.name === targetNode) { targetInputSchemaName = j.input; break; } }
    if (!targetInputSchemaName) for (const h of document.humans) { if (h.name === targetNode) { targetInputSchemaName = h.input; break; } }
    if (targetInputSchemaName) {
      const schema = document.schemas.find((s) => s.name === targetInputSchemaName);
      if (schema) {
        for (const f of schema.fields) {
          if (f.name) refs.push({ label: f.name, value: `{{input.${f.name}}}`, group: "input" });
        }
      }
    }

    // {{vars.*}} from top-level and workflow vars
    const varFields = document.vars?.fields ?? [];
    for (const v of varFields) {
      if (v.name) refs.push({ label: v.name, value: `{{vars.${v.name}}}`, group: "vars" });
    }
    const wfVars = activeWorkflow?.vars?.fields ?? [];
    for (const v of wfVars) {
      if (v.name && !varFields.some((f) => f.name === v.name)) {
        refs.push({ label: v.name, value: `{{vars.${v.name}}}`, group: "vars" });
      }
    }

    // {{outputs.*}} from all nodes
    const allNodes: { name: string; output: string }[] = [];
    for (const a of document.agents) allNodes.push({ name: a.name, output: a.output });
    for (const j of document.judges) allNodes.push({ name: j.name, output: j.output });
    for (const h of document.humans) allNodes.push({ name: h.name, output: h.output });
    for (const t of document.tools) allNodes.push({ name: t.name, output: t.output });

    // Collect delegated node names for _session_id suggestions
    const delegatedNodes = new Set<string>();
    for (const a of document.agents) { if (a.delegate) delegatedNodes.add(a.name); }
    for (const j of document.judges) { if (j.delegate) delegatedNodes.add(j.name); }

    for (const node of allNodes) {
      refs.push({ label: node.name, value: `{{outputs.${node.name}}}`, group: "outputs" });
      if (node.output) {
        const schema = document.schemas.find((s) => s.name === node.output);
        if (schema) {
          for (const f of schema.fields) {
            if (f.name) refs.push({ label: `${node.name}.${f.name}`, value: `{{outputs.${node.name}.${f.name}}}`, group: "outputs" });
          }
        }
      }
      // Delegated nodes expose _session_id for session continuity
      if (delegatedNodes.has(node.name)) {
        refs.push({ label: `${node.name}._session_id`, value: `{{outputs.${node.name}._session_id}}`, group: "sessions" });
      }
    }

    // {{artifacts.*}} from nodes with publish
    for (const a of document.agents) { if (a.publish) refs.push({ label: a.publish, value: `{{artifacts.${a.publish}}}`, group: "artifacts" }); }
    for (const j of document.judges) { if (j.publish) refs.push({ label: j.publish, value: `{{artifacts.${j.publish}}}`, group: "artifacts" }); }
    for (const h of document.humans) { if (h.publish) refs.push({ label: h.publish, value: `{{artifacts.${h.publish}}}`, group: "artifacts" }); }

    return refs;
  }, [document, activeWorkflow]);

  const when = edge.when;
  const loop = edge.loop;
  const withEntries = edge.with ?? [];

  return (
    <div className="space-y-3">
      <div>
        <p className="text-xs text-gray-400 mb-1">Connection</p>
        <p className="text-sm text-white">
          {edge.from} <span className="text-gray-500">-&gt;</span> {edge.to}
        </p>
      </div>

      {/* When clause */}
      <div className="border-t border-gray-700 pt-2">
        <div className="flex items-center justify-between mb-1">
          <span className="text-xs text-gray-400 font-semibold">When Condition <span className="text-gray-600 hover:text-gray-300 cursor-help" title="Boolean field from the source node's output schema. Controls whether this edge is followed.">?</span></span>
          {!when ? (
            <button
              className="text-xs text-blue-400 hover:text-blue-300"
              onClick={() => setWhen({ condition: "", negated: false })}
            >
              + Add
            </button>
          ) : (
            <button
              className="text-xs text-red-400 hover:text-red-300"
              onClick={() => setWhen(undefined)}
            >
              Remove
            </button>
          )}
        </div>
        {when && (
          <>
            {boolFieldOptions.length > 0 ? (
              <SelectField
                label="Condition (bool field)"
                value={when.condition}
                onChange={(v) => setWhen({ ...when, condition: v })}
                options={boolFieldOptions}
                allowEmpty
                emptyLabel="-- select field --"
              />
            ) : (
              <TextField
                label="Condition"
                value={when.condition}
                onChange={(v) => setWhen({ ...when, condition: v })}
                placeholder="e.g. approved"
              />
            )}
            <CheckboxField
              label="Negated (when not)"
              checked={when.negated}
              onChange={(v) => setWhen({ ...when, negated: v })}
              help="Invert the condition: follow this edge when the field is false."
            />
          </>
        )}
      </div>

      {/* Loop clause */}
      <div className="border-t border-gray-700 pt-2">
        <div className="flex items-center justify-between mb-1">
          <span className="text-xs text-gray-400 font-semibold">Loop <span className="text-gray-600 hover:text-gray-300 cursor-help" title="Creates a named loop through this edge, repeating up to max_iterations times. Use {{outputs.node.history}} to access previous iterations.">?</span></span>
          {!loop ? (
            <button
              className="text-xs text-blue-400 hover:text-blue-300"
              onClick={() => setLoop({ name: "", max_iterations: 3 })}
            >
              + Add
            </button>
          ) : (
            <button
              className="text-xs text-red-400 hover:text-red-300"
              onClick={() => setLoop(undefined)}
            >
              Remove
            </button>
          )}
        </div>
        {loop && (
          <>
            <TextField
              label="Loop Name"
              value={loop.name}
              onChange={(v) => setLoop({ ...loop, name: v })}
              placeholder="e.g. refine_loop"
            />
            <NumberField
              label="Max Iterations"
              value={loop.max_iterations}
              onChange={(v) => setLoop({ ...loop, max_iterations: v ?? 3 })}
              min={1}
            />
          </>
        )}
      </div>

      {/* With entries */}
      <div className="border-t border-gray-700 pt-2">
        <div className="flex items-center justify-between mb-1">
          <span className="text-xs text-gray-400 font-semibold">With (data mapping) <span className="text-gray-600 hover:text-gray-300 cursor-help" title="Map data to the target node's input fields. Use {{outputs.node.field}}, {{vars.name}}, or {{artifacts.name}} as values.">?</span></span>
          <button
            className="text-xs text-blue-400 hover:text-blue-300"
            onClick={() => setWith([...withEntries, { key: "", value: "" }])}
          >
            + Add
          </button>
        </div>
        {withEntries.map((entry, i) => (
          <WithEntryRow
            key={i}
            entry={entry}
            index={i}
            withEntries={withEntries}
            setWith={setWith}
            templateRefs={templateRefs}
            enumValues={targetEnumMap.get(entry.key)}
          />
        ))}
      </div>

      {/* Delete edge */}
      <div className="border-t border-gray-700 pt-2">
        <button
          className="w-full bg-red-900 hover:bg-red-800 text-red-200 text-xs py-1 rounded"
          onClick={() => removeEdge(workflowName, edgeIndex, edge.from, edge.to)}
        >
          Delete Edge
        </button>
      </div>
    </div>
  );
}

/** A single With entry row with an "Insert ref" picker for the value field. */
function WithEntryRow({
  entry,
  index,
  withEntries,
  setWith,
  templateRefs,
  enumValues,
}: {
  entry: WithEntry;
  index: number;
  withEntries: WithEntry[];
  setWith: (w: WithEntry[] | undefined) => void;
  templateRefs: { label: string; value: string; group: string }[];
  enumValues?: string[];
}) {
  const [pickerOpen, setPickerOpen] = useState(false);

  const updateValue = useCallback(
    (v: string) => {
      const next = [...withEntries];
      next[index] = { key: entry.key, value: v };
      setWith(next.length > 0 ? next : undefined);
    },
    [withEntries, index, entry.key, setWith],
  );

  const groups = useMemo(() => {
    const map = new Map<string, { label: string; value: string }[]>();
    for (const ref of templateRefs) {
      if (!map.has(ref.group)) map.set(ref.group, []);
      map.get(ref.group)!.push({ label: ref.label, value: ref.value });
    }
    return map;
  }, [templateRefs]);

  return (
    <div className="mb-2 p-1.5 bg-gray-800/50 rounded border border-gray-700">
      <div className="flex gap-1 items-end">
        <div className="flex-1">
          <CommittedTextField
            label="Key"
            value={entry.key}
            onChange={(v) => {
              const next = [...withEntries];
              next[index] = { key: v, value: entry.value };
              setWith(next.length > 0 ? next : undefined);
            }}
            placeholder="target_field"
            validate={(v) => (!v.trim() ? "Key cannot be empty" : null)}
          />
        </div>
        <button
          className="text-red-400 hover:text-red-300 text-xs pb-2"
          onClick={() => {
            const next = withEntries.filter((_, j) => j !== index);
            setWith(next.length > 0 ? next : undefined);
          }}
        >
          x
        </button>
      </div>
      <div className="flex gap-1 items-end">
        <div className="flex-1">
          <TextField
            label="Value"
            value={entry.value}
            onChange={updateValue}
            placeholder="{{outputs.node.field}}"
          />
        </div>
        <div className="relative">
          <button
            className="text-blue-400 hover:text-blue-300 text-[10px] pb-2 whitespace-nowrap"
            onClick={() => setPickerOpen(!pickerOpen)}
            title="Insert a template reference"
          >
            {"{{"} ref
          </button>
          {pickerOpen && (
            <div className="absolute bottom-6 right-0 bg-gray-800 border border-gray-600 rounded-lg shadow-xl z-50 py-1 min-w-[200px] max-h-[240px] overflow-y-auto">
              {Array.from(groups.entries()).map(([group, refs]) => (
                <div key={group}>
                  <div className="px-2 py-1 text-[9px] text-gray-500 uppercase tracking-wider sticky top-0 bg-gray-800">
                    {group}
                  </div>
                  {refs.map((ref) => (
                    <button
                      key={ref.value}
                      className="w-full text-left px-2 py-1 hover:bg-gray-700 text-[11px] text-gray-300 truncate"
                      onClick={() => {
                        updateValue(entry.value ? `${entry.value} ${ref.value}` : ref.value);
                        setPickerOpen(false);
                      }}
                      title={ref.value}
                    >
                      {ref.label}
                    </button>
                  ))}
                </div>
              ))}
              {templateRefs.length === 0 && (
                <p className="px-2 py-1 text-[10px] text-gray-500">No references available. Add nodes with output schemas first.</p>
              )}
            </div>
          )}
        </div>
      </div>
      {enumValues && enumValues.length > 0 && (
        <div className="mt-0.5">
          <span className="text-[9px] text-gray-500">Allowed values: </span>
          <div className="flex flex-wrap gap-1 mt-0.5">
            {enumValues.map((v) => (
              <button
                key={v}
                className="text-[10px] bg-gray-700 hover:bg-gray-600 text-amber-300 px-1.5 py-0.5 rounded cursor-pointer"
                onClick={() => updateValue(`"${v}"`)}
                title={`Set value to "${v}"`}
              >
                {v}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
