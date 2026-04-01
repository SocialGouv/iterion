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
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: "#E67E2222", borderLeft: "3px solid #E67E22" }}
      >
        <span className="text-base">{"\u{1F504}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: "#E67E22" }}>Router</span>
      </div>
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
