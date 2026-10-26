import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import type { BudgetBlock, InteractionMode, WorktreeMode } from "@/api/types";
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
  PERMISSION_OPTIONS,
  WORKFLOW_PERMISSION_HELP,
  WORKFLOW_PERMISSION_RULES_HELP,
  WORKTREE_OPTIONS,
  WORKTREE_HELP,
} from "@/lib/dslOptions";
import { IconButton } from "@/components/ui";
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

  // Named counting semaphores (resource → capacity). Edited as rows; a node's
  // `needs:` acquires a slot. Stored on the workflow as a plain object.
  const resources = workflow.resources ?? {};
  const setResources = (next: Record<string, number>) => {
    const clean = Object.fromEntries(
      Object.entries(next).filter(([k, v]) => k.trim() !== "" && v > 0),
    );
    updateWorkflow(workflow.name, {
      resources: Object.keys(clean).length > 0 ? clean : undefined,
    });
  };
  const renameResource = (oldName: string, newName: string) => {
    if (newName === oldName) return;
    const next: Record<string, number> = {};
    for (const [k, v] of Object.entries(resources)) next[k === oldName ? newName : k] = v;
    setResources(next);
  };
  const addResource = () => {
    let i = 1;
    let name = "resource";
    while (resources[name] !== undefined) name = `resource${i++}`;
    setResources({ ...resources, [name]: 1 });
  };

  return (
    <div className="p-3 text-sm">
      <h2 className="font-bold text-fg-muted mb-3">Workflow Settings</h2>

      <CommittedTextField
        label="Workflow Name"
        required
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
      <SelectField
        label="Worktree"
        value={workflow.worktree ?? ""}
        onChange={(v) =>
          updateWorkflow(workflow.name, {
            worktree: (v || undefined) as WorktreeMode | undefined,
          })
        }
        options={[{ value: "", label: "(unset · run in place)" }, ...WORKTREE_OPTIONS]}
        help={WORKTREE_HELP}
      />
      <TagListField
        label="Tool Policy"
        values={workflow.tool_policy ?? []}
        onChange={(v) => updateWorkflow(workflow.name, { tool_policy: v.length > 0 ? v : undefined })}
        placeholder="Add tool reference..."
      />
      {/* Tool-permission gate (docs/permissions.md): the mode and the three
          rule lists this workflow's agent and judge nodes inherit — and so
          does the recovery agent a tool node's Verified Action dispatches at
          its agent rung, which declares nothing of its own. A plain tool
          node's `command:` is not gated, and its own `permission:` is not
          enforced (C112). An emptied list is written back as `undefined`,
          never `[]`: `[]` reaches the save as a non-nil empty slice, which
          `ir.SameProgram` reads as a different program, so the file cannot
          be saved at all — 422, with a message that names no field. */}
      <SelectField
        label="Permission"
        // C110 accepts the mode case-insensitively and whitespace-trimmed,
        // and the writer puts it back verbatim, so `permission: Deny` is a
        // legal, diagnostic-free spelling. Matched raw it falls through to
        // the empty option and the inspector would report a hard-denying
        // workflow as declaring no gate.
        value={(workflow.permission ?? "").trim().toLowerCase()}
        onChange={(v) => updateWorkflow(workflow.name, { permission: v || undefined })}
        options={PERMISSION_OPTIONS}
        allowEmpty
        emptyLabel="-- unset · ITERION_PERMISSION, else off --"
        help={WORKFLOW_PERMISSION_HELP}
      />
      <TagListField
        label="Allow"
        values={workflow.allow ?? []}
        onChange={(v) => updateWorkflow(workflow.name, { allow: v.length > 0 ? v : undefined })}
        literalValues
        placeholder="Add allow rule, e.g. Read(**)..."
        help={WORKFLOW_PERMISSION_RULES_HELP}
      />
      <TagListField
        label="Ask"
        values={workflow.ask ?? []}
        onChange={(v) => updateWorkflow(workflow.name, { ask: v.length > 0 ? v : undefined })}
        literalValues
        placeholder="Add ask rule, e.g. Bash(git push:*)..."
        help={WORKFLOW_PERMISSION_RULES_HELP}
      />
      <TagListField
        label="Deny"
        values={workflow.deny ?? []}
        onChange={(v) => updateWorkflow(workflow.name, { deny: v.length > 0 ? v : undefined })}
        literalValues
        placeholder="Add deny rule, e.g. Read(.env*)..."
        help={WORKFLOW_PERMISSION_RULES_HELP}
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

      <div className="border-t border-border-default mt-3 pt-3">
        <h3 className="text-xs text-fg-subtle font-semibold mb-1">Resources</h3>
        <p className="text-caption text-fg-subtle mb-2">
          Named counting semaphores. A node&apos;s <code>needs:</code> acquires a
          slot — bounds concurrent use of a scarce resource (e.g. Godot sessions)
          without a global parallelism cap.
        </p>
        {Object.entries(resources).length === 0 && (
          <p className="text-caption text-fg-subtle mb-2">No resources defined.</p>
        )}
        {Object.entries(resources).map(([name, cap]) => (
          <div key={name} className="flex items-end gap-2 mb-1">
            <div className="flex-1">
              <CommittedTextField
                label="Name"
                required
                value={name}
                onChange={(v) => renameResource(name, v)}
                validate={(v) => (!v.trim() ? "Name required" : null)}
              />
            </div>
            <div className="w-24">
              <NumberField
                label="Capacity"
                value={cap}
                onChange={(v) => setResources({ ...resources, [name]: v ?? 1 })}
                min={1}
              />
            </div>
            <IconButton
              variant="danger"
              size="sm"
              label="Remove resource"
              onClick={() => {
                const next = { ...resources };
                delete next[name];
                setResources(next);
              }}
              className="mb-0.5"
            >
              ✕
            </IconButton>
          </div>
        ))}
        <button
          type="button"
          onClick={addResource}
          className="text-xs text-info-fg px-2 py-1 mt-1 hover:bg-info-soft rounded border border-info/40"
        >
          + Add resource
        </button>
      </div>
    </div>
  );
}
