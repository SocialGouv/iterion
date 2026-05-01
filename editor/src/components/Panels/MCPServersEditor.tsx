import { useState } from "react";

import { useDocumentStore } from "@/store/document";
import type { MCPServerDecl, MCPTransport } from "@/api/types";

import {
  CommittedTextField,
  SelectField,
  TagListField,
  TextField,
} from "./forms/FormField";

/** Top-level reusable MCP server declarations. Servers listed here are
 *  referenced by name from a node's or workflow's MCP config block. */
export default function MCPServersEditor() {
  const document = useDocumentStore((s) => s.document);
  const addMCPServer = useDocumentStore((s) => s.addMCPServer);
  const removeMCPServer = useDocumentStore((s) => s.removeMCPServer);
  const updateMCPServer = useDocumentStore((s) => s.updateMCPServer);

  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);

  if (!document) return null;
  const servers = document.mcp_servers ?? [];

  const handleAdd = () => {
    const existing = new Set(servers.map((s) => s.name));
    let i = 1;
    while (existing.has(`mcp_server_${i}`)) i++;
    addMCPServer({ name: `mcp_server_${i}`, transport: "stdio", command: "" });
  };

  return (
    <div className="p-3 text-sm">
      <div className="flex items-center justify-between mb-3">
        <h2 className="font-bold text-fg-muted">MCP Servers</h2>
        <button
          type="button"
          onClick={handleAdd}
          className="text-xs px-2 py-1 rounded bg-accent/20 text-accent hover:bg-accent/30"
        >
          + Add server
        </button>
      </div>
      {servers.length === 0 ? (
        <p className="text-xs text-fg-subtle">
          No MCP servers declared. Add a server to expose its tools to agents
          and judges via per-node or workflow MCP config.
        </p>
      ) : (
        <ul className="space-y-3">
          {servers.map((srv) => (
            <li
              key={srv.name}
              className="border border-border-default rounded p-2 bg-surface-1/40"
            >
              <ServerCard
                server={srv}
                allNames={new Set(servers.map((s) => s.name))}
                onRename={(newName) => {
                  // Renaming via update keeps the rest of the fields; the
                  // store's updateInArray matches on the *current* name so
                  // we patch in two steps if the name changes.
                  if (newName === srv.name || !newName.trim()) return;
                  updateMCPServer(srv.name, { name: newName });
                }}
                onPatch={(patch) => updateMCPServer(srv.name, patch)}
                onDelete={() => setConfirmDelete(srv.name)}
              />
            </li>
          ))}
        </ul>
      )}
      {confirmDelete !== null && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-surface-0 border border-border-default rounded p-3 max-w-sm">
            <p className="text-sm mb-3">
              Delete MCP server <code>{confirmDelete}</code>? Nodes referencing
              it by name will fail validation until the reference is removed.
            </p>
            <div className="flex justify-end gap-2">
              <button
                type="button"
                className="text-xs px-2 py-1 rounded bg-surface-2"
                onClick={() => setConfirmDelete(null)}
              >
                Cancel
              </button>
              <button
                type="button"
                className="text-xs px-2 py-1 rounded bg-danger text-on-danger"
                onClick={() => {
                  removeMCPServer(confirmDelete);
                  setConfirmDelete(null);
                }}
              >
                Delete
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function ServerCard({
  server,
  allNames,
  onRename,
  onPatch,
  onDelete,
}: {
  server: MCPServerDecl;
  allNames: Set<string>;
  onRename: (newName: string) => void;
  onPatch: (patch: Partial<MCPServerDecl>) => void;
  onDelete: () => void;
}) {
  const transport = server.transport ?? "stdio";
  return (
    <>
      <div className="flex items-center justify-between mb-1">
        <span className="text-[10px] text-fg-subtle font-mono uppercase">{transport}</span>
        <button
          type="button"
          className="text-[10px] text-danger hover:text-danger-fg"
          onClick={onDelete}
        >
          Remove
        </button>
      </div>
      <CommittedTextField
        label="Name"
        value={server.name}
        onChange={onRename}
        validate={(v) => {
          if (!v.trim()) return "Name cannot be empty";
          if (/\s/.test(v)) return "Name cannot contain spaces";
          const others = new Set(allNames);
          others.delete(server.name);
          if (others.has(v)) return "Name already used by another server";
          return null;
        }}
      />
      <SelectField
        label="Transport"
        value={transport}
        onChange={(v) => onPatch({ transport: v as MCPTransport })}
        options={[
          { value: "stdio", label: "stdio (subprocess)" },
          { value: "http", label: "http" },
          { value: "sse", label: "sse" },
        ]}
      />
      {transport === "stdio" ? (
        <>
          <TextField
            label="Command"
            value={server.command ?? ""}
            onChange={(v) => onPatch({ command: v || undefined })}
            placeholder="e.g. npx @modelcontextprotocol/server-everything"
          />
          <TagListField
            label="Args"
            values={server.args ?? []}
            onChange={(v) => onPatch({ args: v.length > 0 ? v : undefined })}
            placeholder="argument..."
          />
        </>
      ) : (
        <TextField
          label="URL"
          value={server.url ?? ""}
          onChange={(v) => onPatch({ url: v || undefined })}
          placeholder="https://..."
        />
      )}
      <details className="mt-2">
        <summary className="text-xs text-fg-subtle font-semibold cursor-pointer">
          OAuth (optional)
        </summary>
        <div className="pl-2">
          <TextField
            label="Type"
            value={server.auth?.type ?? ""}
            onChange={(v) =>
              onPatch({
                auth: { ...(server.auth ?? {}), type: v || undefined },
              })
            }
            placeholder="oauth2"
          />
          <TextField
            label="Auth URL"
            value={server.auth?.auth_url ?? ""}
            onChange={(v) =>
              onPatch({
                auth: { ...(server.auth ?? {}), auth_url: v || undefined },
              })
            }
          />
          <TextField
            label="Token URL"
            value={server.auth?.token_url ?? ""}
            onChange={(v) =>
              onPatch({
                auth: { ...(server.auth ?? {}), token_url: v || undefined },
              })
            }
          />
          <TextField
            label="Client ID"
            value={server.auth?.client_id ?? ""}
            onChange={(v) =>
              onPatch({
                auth: { ...(server.auth ?? {}), client_id: v || undefined },
              })
            }
          />
          <TagListField
            label="Scopes"
            values={server.auth?.scopes ?? []}
            onChange={(v) =>
              onPatch({
                auth: {
                  ...(server.auth ?? {}),
                  scopes: v.length > 0 ? v : undefined,
                },
              })
            }
          />
        </div>
      </details>
    </>
  );
}
