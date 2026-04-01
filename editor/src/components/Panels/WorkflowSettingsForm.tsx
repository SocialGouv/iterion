import { useDocumentStore } from "@/store/document";
import type { BudgetBlock } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import { TextField, NumberField, SelectField } from "./forms/FormField";

export default function WorkflowSettingsForm() {
  const document = useDocumentStore((s) => s.document);
  const updateWorkflow = useDocumentStore((s) => s.updateWorkflow);
  const updateWorkflowBudget = useDocumentStore((s) => s.updateWorkflowBudget);

  const workflow = document?.workflows?.[0];
  if (!workflow) {
    return <p className="p-3 text-gray-500 text-xs">No workflow defined.</p>;
  }

  const nodeNames = document ? Array.from(getAllNodeNames(document)) : [];
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
      <h2 className="font-bold text-gray-300 mb-3">Workflow Settings</h2>

      <TextField
        label="Workflow Name"
        value={workflow.name}
        onChange={(v) => updateWorkflow(workflow.name, { name: v })}
      />

      <SelectField
        label="Entry Node"
        value={workflow.entry}
        onChange={(v) => updateWorkflow(workflow.name, { entry: v })}
        options={nodeOptions}
        allowEmpty
        emptyLabel="-- select entry node --"
      />

      <div className="border-t border-gray-700 mt-3 pt-3">
        <h3 className="text-xs text-gray-400 font-semibold mb-2">Budget</h3>
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
