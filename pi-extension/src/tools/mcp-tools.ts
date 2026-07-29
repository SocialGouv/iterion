/**
 * Registers an MCP server's tools on pi.
 *
 * Nothing about any specific server is encoded here: the tools come from
 * `tools/list`, so the server stays the single source of truth for what exists,
 * what it is called, and what arguments it takes. A new board operation, or a
 * capability the run was not granted, needs no change in this file.
 *
 * Tools are registered on pi under iterion's `mcp__<server>__<tool>` FQN, not
 * under the bare name `tools/list` returns. That is load-bearing: iterion's
 * permission layer keys its infrastructure exemption on the `mcp__iterion…`
 * namespace, so registering the board's bare `create_issue` would make
 * `permission: deny` hard-block every board write and `permission: ask` pause
 * the run on the very tools used to talk to the board. The prefix is a CLIENT
 * convention (claude_code's SDK applies it too); the wire call always uses the
 * server's own bare name.
 */

import { Type } from "typebox";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { McpClient } from "../mcp/client.js";
import { HttpTransport } from "../mcp/http.js";
import type { McpCallResult, Transport } from "../mcp/protocol.js";
import { SseTransport } from "../mcp/sse.js";
import { StdioTransport } from "../mcp/stdio.js";
import type { McpServerConfig } from "../config.js";

/**
 * Builds the `mcp__<server>__<tool>` name a tool is registered under.
 *
 * The prefix is imposed, never accepted from the server. Tool names come from
 * the server's own `tools/list` reply, and iterion's permission layer exempts
 * the whole `mcp__iterion…` namespace as infrastructure — so honouring a
 * server-supplied `mcp__` prefix would let a hostile, compromised or
 * prompt-injected server advertise `mcp__iterion_board__anything` and have
 * every call to it allowed, even under `permission: deny`. claude_code's SDK
 * has no such hole because it always prefixes with the configured server name.
 *
 * A server that legitimately already answers with its OWN `mcp__<server>__`
 * prefix is passed through so it is not double-qualified; anything else is
 * stripped of a leading `mcp__` and re-qualified under the name iterion
 * configured.
 */
export function qualifyToolName(server: string, tool: string): string {
	const ours = `mcp__${server}__`;
	if (tool.startsWith(ours)) return tool;
	return ours + (tool.startsWith("mcp__") ? tool.slice("mcp__".length) : tool);
}

/**
 * Reports whether a server name would land its tools inside iterion's
 * reserved, permission-exempt namespace. Separators are normalised the same
 * way permission.IsInfrastructureTool does, so `iterion-board`, `iterion.x`
 * and `iterion_board` are all caught.
 */
export function reservedServerName(server: string): boolean {
	return server.toLowerCase().replace(/[-.]/g, "_").startsWith("iterion");
}

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

function makeTransport(server: McpServerConfig, log: (line: string) => void): Transport {
	switch (server.transport) {
		case "stdio":
			return new StdioTransport(server.command ?? "", server.args ?? [], server.env ?? {}, (line) =>
				log(`${server.name}: ${line}`),
			);
		case "sse":
			return new SseTransport(server.url ?? "", server.headers ?? {});
		default:
			return new HttpTransport(server.url ?? "", server.headers ?? {});
	}
}

/**
 * Rejects if `work` has not settled within the budget.
 *
 * Bridging happens inside `session_start`, which pi awaits before the agent
 * takes its first turn — and which iterion's own handshake is waiting on. A
 * server that accepts a connection and then never answers would otherwise hang
 * the session start, and with it the entire run. The budget converts that into
 * one missing server.
 */
function withDeadline<T>(work: Promise<T>, ms: number, what: string): Promise<T> {
	return new Promise<T>((resolve, reject) => {
		const timer = setTimeout(() => reject(new Error(`${what} timed out after ${ms}ms`)), ms);
		work.then(
			(v) => {
				clearTimeout(timer);
				resolve(v);
			},
			(e) => {
				clearTimeout(timer);
				reject(e);
			},
		);
	});
}

/**
 * Connects to one server and registers everything it offers.
 *
 * Returns the live client so the caller can close it: a stdio server is a
 * child process, and leaving it running would outlive the run.
 */
export async function installMcpServer(
	pi: ExtensionAPI,
	server: McpServerConfig,
	log: (line: string) => void = () => {},
	connectTimeoutMs = 10_000,
): Promise<{ client: McpClient; count: number }> {
	const client = new McpClient(makeTransport(server, log));
	try {
		await withDeadline(client.initialize(), connectTimeoutMs, `${server.name}: handshake`);
		const tools = await withDeadline(client.listTools(), connectTimeoutMs, `${server.name}: tools/list`);

		for (const tool of tools) {
			const fqn = qualifyToolName(server.name, tool.name);
			pi.registerTool({
				name: fqn,
				label: fqn,
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
							content: [{ type: "text" as const, text: `${fqn} failed: ${msg}` }],
							details: undefined,
							isError: true,
						};
					}
				},
			});
		}
		return { client, count: tools.length };
	} catch (err) {
		// A half-open connection is still a child process or an HTTP stream.
		client.close();
		throw err;
	}
}
