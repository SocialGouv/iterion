import { useDocumentStore } from "@/store/document";
import type { JoinDecl, JoinStrategy } from "@/api/types";
import { TextField, SelectField, MultiSelectField } from "./FormField";
import { getAllNodeNames } from "@/lib/defaults";

interface Props {
  decl: JoinDecl;
}

export default function JoinForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateJoin = useDocumentStore((s) => s.updateJoin);
  const renameNode = useDocumentStore((s) => s.renameNode);

  const nodeNames = document ? Array.from(getAllNodeNames(document)).filter((n) => n !== decl.name) : [];
  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));

  return (
    <div className="space-y-1">
      <TextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
      />
      <SelectField
        label="Strategy"
        value={decl.strategy}
        onChange={(v) => updateJoin(decl.name, { strategy: v as JoinStrategy })}
        options={[
          { value: "wait_all", label: "wait_all" },
          { value: "best_effort", label: "best_effort" },
        ]}
      />
      <MultiSelectField
        label="Require"
        values={decl.require ?? []}
        onChange={(v) => updateJoin(decl.name, { require: v })}
        options={nodeNames}
      />
      <SelectField
        label="Output Schema"
        value={decl.output}
        onChange={(v) => updateJoin(decl.name, { output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
      />
    </div>
  );
}
