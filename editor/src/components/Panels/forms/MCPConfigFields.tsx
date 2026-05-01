import { useCallback } from "react";

import { useDocumentStore } from "@/store/document";
import type { MCPConfigDecl } from "@/api/types";
import { CheckboxField, TagListField } from "./FormField";

interface Props {
  /** Workflow blocks expose `autoload_project`; node blocks expose
   *  `inherit`. We hide the field that doesn't apply to the scope. */
  scope: "workflow" | "node";
  value: MCPConfigDecl | undefined;
  onChange: (config: MCPConfigDecl | undefined) => void;
}

export default function MCPConfigFields({ scope, value, onChange }: Props) {
  const document = useDocumentStore((s) => s.document);
  const declared = (document?.mcp_servers ?? []).map((s) => s.name);

  const set = useCallback(
    (patch: Partial<MCPConfigDecl>) => {
      const next: MCPConfigDecl = { ...(value ?? {}), ...patch };
      // Empty block → drop the whole field so JSON omits it.
      const empty =
        next.autoload_project === undefined &&
        next.inherit === undefined &&
        (!next.servers || next.servers.length === 0) &&
        (!next.disable || next.disable.length === 0);
      onChange(empty ? undefined : next);
    },
    [value, onChange],
  );

  return (
    <details className="border-t border-border-default pt-2 mt-2">
      <summary className="cursor-pointer text-xs text-fg-subtle font-semibold mb-1">
        MCP {scope === "workflow" ? "(workflow)" : "(node)"}
      </summary>
      <div className="pl-2">
        {declared.length === 0 ? (
          <p className="text-[10px] text-fg-subtle italic mb-1">
            Declare servers in the workspace MCP panel first, then reference them here by name.
          </p>
        ) : (
          <p className="text-[10px] text-fg-subtle mb-1">
            Available: {declared.map((n) => `@${n}`).join(", ")}
          </p>
        )}
        {scope === "workflow" ? (
          <CheckboxField
            label="Autoload project servers"
            checked={!!value?.autoload_project}
            onChange={(v) => set({ autoload_project: v ? true : undefined })}
            help="Pull in MCP servers declared at the project level (.iterion config)."
          />
        ) : (
          <CheckboxField
            label="Inherit workflow servers"
            checked={value?.inherit !== false}
            onChange={(v) => set({ inherit: v ? undefined : false })}
            help="When unchecked, this node ignores the workflow-level MCP set and only sees `Servers` listed below."
          />
        )}
        <TagListField
          label="Servers (allow)"
          values={value?.servers ?? []}
          onChange={(v) => set({ servers: v.length > 0 ? v : undefined })}
          placeholder="server name..."
        />
        <TagListField
          label="Servers (disable)"
          values={value?.disable ?? []}
          onChange={(v) => set({ disable: v.length > 0 ? v : undefined })}
          placeholder="server name..."
        />
      </div>
    </details>
  );
}
