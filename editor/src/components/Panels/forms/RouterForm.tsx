import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import type { RouterDecl, RouterMode } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import { CommittedTextField, SelectField } from "./FormField";

interface Props {
  decl: RouterDecl;
}

export default function RouterForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateRouter = useDocumentStore((s) => s.updateRouter);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: "#E67E2222", borderLeft: "3px solid #E67E22" }}
      >
        <span className="text-base">{"\u{1F504}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: "#E67E22" }}>Router</span>
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
