/**
 * A minimal MCP client over HTTP JSON-RPC.
 *
 * pi has no MCP client at all, so every MCP surface iterion built — the board,
 * ask_user, and any `mcp_server:` a workflow declares — is unreachable from a
 * pi node. This is the smallest thing that changes that.
 *
 * It is deliberately hand-rolled rather than pulled from the MCP SDK: the
 * server side here is iterion's own, the transport is a single POST, and the
 * extension has to stay a dependency-free single bundle that loads inside a
 * sandbox with no node_modules. When stdio and SSE transports arrive for
 * third-party servers, this stays the streamable-HTTP half.
 *
 * Tools are DISCOVERED, never hardcoded: `tools/list` is the source of truth,
 * so the server's capability gating and schemas remain authoritative and a new
 * board operation needs no change here.
 */

export interface McpTool {
	name: string;
	description?: string;
	inputSchema?: Record<string, unknown>;
}

export interface McpContent {
	type: string;
	text?: string;
	[k: string]: unknown;
}

export interface McpCallResult {
	content?: McpContent[];
	isError?: boolean;
	[k: string]: unknown;
}

interface JsonRpcResponse<T> {
	jsonrpc?: string;
	id?: unknown;
	result?: T;
	error?: { code: number; message: string };
}

export class McpHttpClient {
	private nextId = 0;

	constructor(
		private readonly url: string,
		private readonly headers: Record<string, string> = {},
		private readonly timeoutMs = 60_000,
	) {}

	/**
	 * MCP requires an `initialize` handshake before anything else. iterion's
	 * board server is lenient, but a third-party server is not, and doing it
	 * here keeps this client honest against both.
	 */
	async initialize(): Promise<void> {
		await this.rpc<unknown>("initialize", {
			protocolVersion: "2024-11-05",
			capabilities: {},
			clientInfo: { name: "iterion-pi-extension", version: "1" },
		});
	}

	async listTools(): Promise<McpTool[]> {
		const res = await this.rpc<{ tools?: McpTool[] }>("tools/list", {});
		return res?.tools ?? [];
	}

	async callTool(name: string, args: unknown): Promise<McpCallResult> {
		return (await this.rpc<McpCallResult>("tools/call", { name, arguments: args })) ?? {};
	}

	private async rpc<T>(method: string, params: unknown): Promise<T | undefined> {
		this.nextId += 1;
		const controller = new AbortController();
		const timer = setTimeout(() => controller.abort(), this.timeoutMs);
		try {
			const res = await fetch(this.url, {
				method: "POST",
				headers: { "content-type": "application/json", ...this.headers },
				body: JSON.stringify({ jsonrpc: "2.0", id: this.nextId, method, params }),
				signal: controller.signal,
			});
			if (!res.ok) {
				throw new Error(`${method}: HTTP ${res.status} ${res.statusText}`);
			}
			const body = (await res.json()) as JsonRpcResponse<T>;
			if (body.error) {
				throw new Error(`${method}: ${body.error.message} (code ${body.error.code})`);
			}
			return body.result;
		} finally {
			clearTimeout(timer);
		}
	}
}
