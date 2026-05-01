import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import type { BudgetBlock, InteractionMode } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import {
  TextField,
  CommittedTextField,
  NumberField,
  SelectField,
  TagListField,
} from "./forms/FormField";
import {
  BACKEND_OPTIONS,
  INTERACTION_OPTIONS,
} from "@/lib/dslOptions";
import CompactionFields from "./forms/CompactionFields";
import MCPConfigFields from "./forms/MCPConfigFields";

export default function WorkflowSettingsForm() {
  const document = useDocumentStore((s) => s.document);
  const updateWorkflow = useDocumentStore((s) => s.updateWorkflow);
  const updateWorkflowBudget = useDocumentStore((s) => s.updateWorkflowBudget);
  const updateWorkflowCompaction = useDocumentStore(
    (s) => s.updateWorkflowCompaction,
  );
  const setActiveWorkflowName = useUIStore((s) => s.setActiveWorkflowName);

  const workflow = useActiveWorkflow();
  if (!workflow) {
    return <p className="p-3 text-fg-subtle text-xs">No workflow defined.</p>;
  }

  const nodeNames = document ? Array.from(getAllNodeNames(document)).filter((n) => n !== "done" && n !== "fail") : [];
  const nodeOptions = nodeNames.map((n) => ({ value: n, label: n }));
  const budget = workflow.budget ?? {};

  const setBudgetField = (field: keyof BudgetBlock, value: number | string | undefined) => {
    const next = { ...budget, [field]: value };
    // Clean up undefined fields
    const clean = Object.fromEntries(Object.entries(next).filter(([, v]) => v !== undefined && v !== "")) as BudgetBlock;
    updateWorkflowBudget(workflow.name, Object.keys(clean).length > 0 ? clean : undefined);
  };

  return (
    <div className="p-3 text-sm">
      <h2 className="font-bold text-fg-muted mb-3">Workflow Settings</h2>

      <CommittedTextField
        label="Workflow Name"
        value={workflow.name}
        onChange={(v) => { updateWorkflow(workflow.name, { name: v }); setActiveWorkflowName(v); }}
        validate={(v) => {
          if (!v.trim()) return "Name cannot be empty";
          const existing = new Set((document?.workflows ?? []).map((w) => w.name));
          existing.delete(workflow.name);
          if (existing.has(v)) return "Workflow name already exists";
          return null;
        }}
      />

      <SelectField
        label="Entry Node"
        value={workflow.entry}
        onChange={(v) => updateWorkflow(workflow.name, { entry: v })}
        options={nodeOptions}
        allowEmpty
        emptyLabel="-- select entry node --"
      />

      <SelectField
        label="Default Backend"
        value={workflow.default_backend ?? ""}
        onChange={(v) => updateWorkflow(workflow.name, { default_backend: v || undefined })}
        // Workflow-level: empty means "per-node default" (override whatever
        // the node sets), not "direct LLM API".
        options={BACKEND_OPTIONS.map((o) =>
          o.value === "" ? { value: "", label: "(per-node default)" } : o,
        )}
        help="Backend used by any node that doesn't set its own."
      />
      <SelectField
        label="Default Interaction"
        value={workflow.interaction ?? ""}
        onChange={(v) =>
          updateWorkflow(workflow.name, {
            interaction: (v || undefined) as InteractionMode | undefined,
          })
        }
        options={[{ value: "", label: "(per-node default)" }, ...INTERACTION_OPTIONS]}
        help="Default interaction mode for ask_user / human-in-the-loop requests in this workflow."
      />
      <TagListField
        label="Tool Policy"
        values={workflow.tool_policy ?? []}
        onChange={(v) => updateWorkflow(workflow.name, { tool_policy: v.length > 0 ? v : undefined })}
        placeholder="Add allow/deny pattern..."
      />

      <CompactionFields
        value={workflow.compaction}
        onChange={(c) => updateWorkflowCompaction(workflow.name, c)}
      />
      <MCPConfigFields
        scope="workflow"
        value={workflow.mcp}
        onChange={(c) => updateWorkflow(workflow.name, { mcp: c })}
      />

      <div className="border-t border-border-default mt-3 pt-3">
        <h3 className="text-xs text-fg-subtle font-semibold mb-2">Budget</h3>
        <NumberField
          label="Max Parallel Branches"
          value={budget.max_parallel_branches}
          onChange={(v) => setBudgetField("max_parallel_branches", v)}
          min={1}
        />
        <TextField
          label="Max Duration"
          value={budget.max_duration ?? ""}
          onChange={(v) => setBudgetField("max_duration", v || undefined)}
          placeholder="e.g. 60m"
        />
        <NumberField
          label="Max Cost (USD)"
          value={budget.max_cost_usd}
          onChange={(v) => setBudgetField("max_cost_usd", v)}
          min={0}
        />
        <NumberField
          label="Max Tokens"
          value={budget.max_tokens}
          onChange={(v) => setBudgetField("max_tokens", v)}
          min={0}
        />
        <NumberField
          label="Max Iterations"
          value={budget.max_iterations}
          onChange={(v) => setBudgetField("max_iterations", v)}
          min={1}
        />
      </div>
    </div>
  );
}
