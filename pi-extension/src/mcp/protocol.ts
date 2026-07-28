/**
 * MCP wire types and the transport seam.
 *
 * MCP is one JSON-RPC 2.0 conversation carried over interchangeable
 * transports. Splitting the two apart is what lets `initialize` / `tools/list`
 * / `tools/call` be written once and work against a subprocess, a streamable
 * HTTP endpoint, or a legacy SSE stream.
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

/** One JSON-RPC frame, in either direction. */
export interface JsonRpcMessage {
	jsonrpc?: string;
	id?: number | string | null;
	method?: string;
	params?: unknown;
	result?: unknown;
	error?: { code: number; message: string; data?: unknown };
}

/**
 * A bidirectional channel to one MCP server.
 *
 * `send` is deliberately not request/response: an HTTP POST answers inline, a
 * subprocess answers whenever it likes, and a legacy SSE server answers on a
 * stream opened before the request existed. All three reduce to "put a frame
 * in, get frames out", so correlation lives once in the client above.
 */
export interface Transport {
	/** Establish the channel. Must be safe to call exactly once. */
	start(): Promise<void>;
	/** Deliver one frame to the server. Rejects if it cannot be delivered. */
	send(message: JsonRpcMessage): Promise<void>;
	/** Register the sink for frames arriving from the server. */
	onMessage(handler: (message: JsonRpcMessage) => void): void;
	/** Release every resource. Must be idempotent and must never throw. */
	close(): void;
}
