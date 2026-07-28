/**
 * Registers an MCP server's tools on pi.
 *
 * Nothing about any specific server is encoded here: the tools come from
 * `tools/list`, so the server stays the single source of truth for what exists,
 * what it is called, and what arguments it takes. A new board operation, or a
 * capability the run was not granted, needs no change in this file.
 *
 * The names are kept in iterion's `mcp__<server>__<tool>` shape rather than
 * simplified. That is load-bearing: iterion's permission layer treats that
 * namespace as infrastructure and exempts it, so renaming the tools would make
 * `permission: ask` pause the run on the very tools used to talk to the board.
 */

import { Type } from "typebox";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { McpHttpClient, type McpCallResult } from "../mcp/http.js";
import type { McpServerConfig } from "../config.js";

/** Renders an MCP tool result as pi tool content. */
function renderResult(result: McpCallResult): { text: string; isError: boolean } {
	const parts: string[] = [];
	for (const c of result.content ?? []) {
		if (typeof c.text === "string") parts.push(c.text);
	}
	// A result with no text block is not an error — it just has nothing to
	// say. Showing the raw payload beats showing nothing.
	const text = parts.length > 0 ? parts.join("\n") : JSON.stringify(result);
	return { text, isError: result.isError === true };
}

export async function installMcpServer(pi: ExtensionAPI, server: McpServerConfig): Promise<number> {
	if (!server.url) return 0;

	const client = new McpHttpClient(server.url, server.headers ?? {});
	await client.initialize();
	const tools = await client.listTools();

	for (const tool of tools) {
		pi.registerTool({
			name: tool.name,
			label: tool.name,
			description: tool.description ?? `${tool.name} (via ${server.name})`,
			// The server's JSON Schema is passed through unvalidated on this
			// side: it is authoritative, and re-deriving a TypeBox schema from
			// it would only add a second place for the shape to be wrong.
			parameters: Type.Unsafe<Record<string, unknown>>(
				(tool.inputSchema as Record<string, unknown>) ?? { type: "object" },
			),
			async execute(_toolCallId, params) {
				try {
					const result = await client.callTool(tool.name, params ?? {});
					const { text, isError } = renderResult(result);
					return { content: [{ type: "text" as const, text }], details: undefined, isError };
				} catch (err) {
					// Surface the cause to the model: an agent told only
					// "failed" will retry the same call, whereas one told the
					// capability was denied can adapt.
					const msg = err instanceof Error ? err.message : String(err);
					return {
						content: [{ type: "text" as const, text: `${tool.name} failed: ${msg}` }],
						details: undefined,
						isError: true,
					};
				}
			},
		});
	}
	return tools.length;
}
