import { useCallback, useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import type { Edge, WhenClause, LoopClause, WithEntry } from "@/api/types";
import { TextField, NumberField, CheckboxField, SelectField, CommittedTextField } from "./FormField";
import type { RefContext } from "@/lib/refCompletion";

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

  const refContext = useMemo<RefContext>(
    () => ({ kind: "edge-with", edgeFrom: edge.from, edgeTo: edge.to }),
    [edge.from, edge.to],
  );

  const when = edge.when;
  const loop = edge.loop;
  const withEntries = edge.with ?? [];

  return (
    <div className="space-y-3">
      <div>
        <p className="text-xs text-fg-subtle mb-1">Connection</p>
        <p className="text-sm text-fg-default">
          {edge.from} <span className="text-fg-subtle">-&gt;</span> {edge.to}
        </p>
      </div>

      {/* When clause */}
      <div className="border-t border-border-default pt-2">
        <div className="flex items-center justify-between mb-1">
          <span className="text-xs text-fg-subtle font-semibold">When Condition <span className="text-fg-subtle hover:text-fg-muted cursor-help" title="Boolean field from the source node's output schema. Controls whether this edge is followed.">?</span></span>
          {!when ? (
            <button
              className="text-xs text-accent hover:text-accent"
              onClick={() => setWhen({ condition: "", negated: false })}
            >
              + Add
            </button>
          ) : (
            <button
              className="text-xs text-danger hover:text-danger-fg"
              onClick={() => setWhen(undefined)}
            >
              Remove
            </button>
          )}
        </div>
        {when && (
          <>
            <div className="flex gap-2 text-xs">
              <button
                type="button"
                className={`px-2 py-0.5 rounded ${
                  !when.expr ? "bg-accent text-on-accent" : "bg-surface-2 hover:bg-surface-3"
                }`}
                onClick={() =>
                  setWhen({
                    condition: when.condition ?? "",
                    negated: when.negated ?? false,
                  })
                }
              >
                Field
              </button>
              <button
                type="button"
                className={`px-2 py-0.5 rounded ${
                  when.expr ? "bg-accent text-on-accent" : "bg-surface-2 hover:bg-surface-3"
                }`}
                onClick={() => setWhen({ expr: when.expr ?? "" })}
                title="Switch to a raw boolean expression (e.g. approved && loop.l.previous_output.x)"
              >
                Expression
              </button>
            </div>
            {when.expr !== undefined ? (
              <TextField
                label="Expression"
                value={when.expr}
                onChange={(v) => setWhen({ expr: v })}
                placeholder="e.g. approved && loop.l.previous_output.x"
              />
            ) : boolFieldOptions.length > 0 ? (
              <>
                <SelectField
                  label="Condition (bool field)"
                  value={when.condition ?? ""}
                  onChange={(v) => setWhen({ ...when, condition: v })}
                  options={boolFieldOptions}
                  allowEmpty
                  emptyLabel="-- select field --"
                />
                <CheckboxField
                  label="Negated (when not)"
                  checked={when.negated ?? false}
                  onChange={(v) => setWhen({ ...when, negated: v })}
                  help="Invert the condition: follow this edge when the field is false."
                />
              </>
            ) : (
              <>
                <TextField
                  label="Condition"
                  value={when.condition ?? ""}
                  onChange={(v) => setWhen({ ...when, condition: v })}
                  placeholder="e.g. approved"
                />
                <CheckboxField
                  label="Negated (when not)"
                  checked={when.negated ?? false}
                  onChange={(v) => setWhen({ ...when, negated: v })}
                  help="Invert the condition: follow this edge when the field is false."
                />
              </>
            )}
          </>
        )}
      </div>

      {/* Loop clause */}
      <div className="border-t border-border-default pt-2">
        <div className="flex items-center justify-between mb-1">
          <span className="text-xs text-fg-subtle font-semibold">Loop <span className="text-fg-subtle hover:text-fg-muted cursor-help" title="Creates a named loop through this edge, repeating up to max_iterations times. Use {{outputs.node.history}} to access previous iterations.">?</span></span>
          {!loop ? (
            <button
              className="text-xs text-accent hover:text-accent"
              onClick={() => setLoop({ name: "", max_iterations: 3 })}
            >
              + Add
            </button>
          ) : (
            <button
              className="text-xs text-danger hover:text-danger-fg"
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
      <div className="border-t border-border-default pt-2">
        <div className="flex items-center justify-between mb-1">
          <span className="text-xs text-fg-subtle font-semibold">With (data mapping) <span className="text-fg-subtle hover:text-fg-muted cursor-help" title="Map data to the target node's input fields. Use {{outputs.node.field}}, {{vars.name}}, or {{artifacts.name}} as values.">?</span></span>
          <button
            className="text-xs text-accent hover:text-accent"
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
            refContext={refContext}
            enumValues={targetEnumMap.get(entry.key)}
          />
        ))}
      </div>

      {/* Delete edge */}
      <div className="border-t border-border-default pt-2">
        <button
          className="w-full bg-danger-soft hover:bg-danger text-danger-fg text-xs py-1 rounded"
          onClick={() => removeEdge(workflowName, edgeIndex, edge.from, edge.to)}
        >
          Delete Edge
        </button>
      </div>
    </div>
  );
}

function WithEntryRow({
  entry,
  index,
  withEntries,
  setWith,
  refContext,
  enumValues,
}: {
  entry: WithEntry;
  index: number;
  withEntries: WithEntry[];
  setWith: (w: WithEntry[] | undefined) => void;
  refContext: RefContext;
  enumValues?: string[];
}) {
  const updateValue = useCallback(
    (v: string) => {
      const next = [...withEntries];
      next[index] = { key: entry.key, value: v };
      setWith(next.length > 0 ? next : undefined);
    },
    [withEntries, index, entry.key, setWith],
  );

  return (
    <div className="mb-2 p-1.5 bg-surface-1/50 rounded border border-border-default">
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
          className="text-danger hover:text-danger-fg text-xs pb-2"
          onClick={() => {
            const next = withEntries.filter((_, j) => j !== index);
            setWith(next.length > 0 ? next : undefined);
          }}
        >
          x
        </button>
      </div>
      <TextField
        label="Value"
        value={entry.value}
        onChange={updateValue}
        placeholder="{{outputs.node.field}}"
        refContext={refContext}
        help="Type {{ to autocomplete from inputs, vars, outputs, and artifacts in scope."
      />
      {enumValues && enumValues.length > 0 && (
        <div className="mt-0.5">
          <span className="text-[9px] text-fg-subtle">Allowed values: </span>
          <div className="flex flex-wrap gap-1 mt-0.5">
            {enumValues.map((v) => (
              <button
                key={v}
                className="text-[10px] bg-surface-2 hover:bg-surface-3 text-warning-fg px-1.5 py-0.5 rounded cursor-pointer"
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
