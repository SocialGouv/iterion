import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import type { Edge, WhenClause, LoopClause, WithEntry } from "@/api/types";
import { TextField, NumberField, CheckboxField } from "./FormField";

interface Props {
  edge: Edge;
  edgeIndex: number;
  workflowName: string;
}

export default function EdgeForm({ edge, edgeIndex, workflowName }: Props) {
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
          <span className="text-xs text-gray-400 font-semibold">When Condition</span>
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
            <TextField
              label="Condition"
              value={when.condition}
              onChange={(v) => setWhen({ ...when, condition: v })}
              placeholder="e.g. approved"
            />
            <CheckboxField
              label="Negated (when not)"
              checked={when.negated}
              onChange={(v) => setWhen({ ...when, negated: v })}
            />
          </>
        )}
      </div>

      {/* Loop clause */}
      <div className="border-t border-gray-700 pt-2">
        <div className="flex items-center justify-between mb-1">
          <span className="text-xs text-gray-400 font-semibold">Loop</span>
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
          <span className="text-xs text-gray-400 font-semibold">With (data mapping)</span>
          <button
            className="text-xs text-blue-400 hover:text-blue-300"
            onClick={() => setWith([...withEntries, { key: "", value: "" }])}
          >
            + Add
          </button>
        </div>
        {withEntries.map((entry, i) => (
          <div key={i} className="flex gap-1 mb-1 items-end">
            <div className="flex-1">
              <TextField
                label="Key"
                value={entry.key}
                onChange={(v) => {
                  const next = [...withEntries];
                  next[i] = { key: v, value: entry.value };
                  setWith(next.length > 0 ? next : undefined);
                }}
                placeholder="target_field"
              />
            </div>
            <div className="flex-1">
              <TextField
                label="Value"
                value={entry.value}
                onChange={(v) => {
                  const next = [...withEntries];
                  next[i] = { key: entry.key, value: v };
                  setWith(next.length > 0 ? next : undefined);
                }}
                placeholder='{{outputs.node.field}}'
              />
            </div>
            <button
              className="text-red-400 hover:text-red-300 text-xs pb-2"
              onClick={() => {
                const next = withEntries.filter((_, j) => j !== i);
                setWith(next.length > 0 ? next : undefined);
              }}
            >
              x
            </button>
          </div>
        ))}
      </div>

      {/* Delete edge */}
      <div className="border-t border-gray-700 pt-2">
        <button
          className="w-full bg-red-900 hover:bg-red-800 text-red-200 text-xs py-1 rounded"
          onClick={() => removeEdge(workflowName, edgeIndex)}
        >
          Delete Edge
        </button>
      </div>
    </div>
  );
}
