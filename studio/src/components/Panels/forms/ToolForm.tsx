import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useSchemaPromptCreators } from "@/hooks/useSchemaPromptCreators";
import type { ToolNodeDecl, AwaitMode } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import { AWAIT_HELP, AWAIT_OPTIONS } from "@/lib/dslOptions";
import { NODE_COLORS, softColor } from "@/lib/constants";
import { TextField, CommittedTextField, SelectField, SelectFieldWithCreate, TagListField } from "./FormField";

interface Props {
  decl: ToolNodeDecl;
}

export default function ToolForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateTool = useDocumentStore((s) => s.updateTool);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const { createSchema } = useSchemaPromptCreators();

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: softColor(NODE_COLORS.tool), borderLeft: `3px solid ${NODE_COLORS.tool}` }}
      >
        <span className="text-base">{"\u{1F527}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: NODE_COLORS.tool }}>Tool</span>
      </div>
      <CommittedTextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
        onCommit={(v) => setSelectedNode(v)}
        validate={(v) => {
          if (!v.trim()) return "Name cannot be empty";
          if (/\s/.test(v)) return "Name cannot contain spaces";
          const names = getAllNodeNames(document!);
          names.delete(decl.name);
          if (names.has(v)) return "Name already exists";
          return null;
        }}
      />
      <TextField
        label="Command"
        value={decl.command}
        onChange={(v) => updateTool(decl.name, { command: v })}
        placeholder="Shell command (e.g. ${CMD})"
        help="Shell command to execute. Use ${ENV_VAR} for environment variable substitution. Type {{ to autocomplete refs."
        refContext={{ kind: "node-prompt", nodeId: decl.name }}
      />
      <SelectFieldWithCreate
        label="Input Schema"
        value={decl.input ?? ""}
        onChange={(v) => updateTool(decl.name, { input: v || undefined })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- none --"
        onCreate={createSchema}
        help="Optional. When set, structured input is rendered into the command via {{input.field}} templates."
      />
      <SelectFieldWithCreate
        label="Output Schema"
        value={decl.output}
        onChange={(v) => updateTool(decl.name, { output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
        onCreate={createSchema}
      />
      <TextField
        label="Publish"
        value={decl.publish ?? ""}
        onChange={(v) => updateTool(decl.name, { publish: v || undefined })}
        placeholder="Artifact name"
        help="Optional. Publish this node's output as a persistent artifact, accessible downstream via {{artifacts.name}}."
      />
      <SelectField
        label="Await"
        value={decl.await ?? "none"}
        onChange={(v) => updateTool(decl.name, { await: (v === "none" ? undefined : v) as AwaitMode | undefined })}
        options={AWAIT_OPTIONS}
        help={AWAIT_HELP}
      />
      <TagListField
        label="Needs (resources)"
        values={decl.needs ?? []}
        onChange={(v) => updateTool(decl.name, { needs: v.length > 0 ? v : undefined })}
        placeholder="Add resource name (e.g. godot)..."
      />
    </div>
  );
}
