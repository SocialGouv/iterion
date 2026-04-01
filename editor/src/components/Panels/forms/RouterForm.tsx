import { useDocumentStore } from "@/store/document";
import type { RouterDecl, RouterMode } from "@/api/types";
import { TextField, SelectField } from "./FormField";

interface Props {
  decl: RouterDecl;
}

export default function RouterForm({ decl }: Props) {
  const updateRouter = useDocumentStore((s) => s.updateRouter);
  const renameNode = useDocumentStore((s) => s.renameNode);

  return (
    <div className="space-y-1">
      <TextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
      />
      <SelectField
        label="Mode"
        value={decl.mode}
        onChange={(v) => updateRouter(decl.name, { mode: v as RouterMode })}
        options={[
          { value: "fan_out_all", label: "fan_out_all" },
          { value: "condition", label: "condition" },
        ]}
      />
    </div>
  );
}
