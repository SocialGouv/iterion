/**
 * An MCP client: the JSON-RPC conversation, over any transport.
 *
 * Deliberately hand-rolled rather than pulled from the MCP SDK. The extension
 * ships as one bundled file embedded in the iterion binary, so every dependency
 * is bytes committed to the repo and a version that can drift from the engine
 * that drives it. What MCP actually needs here — initialize, tools/list,
 * tools/call — is a few dozen lines over a transport seam.
 *
 * Tools are DISCOVERED, never hardcoded: `tools/list` is the source of truth,
 * so a server's capability gating and schemas stay authoritative and a new
 * board operation needs no change here.
 */

import type { JsonRpcMessage, McpCallResult, McpTool, Transport } from "./protocol.js";

interface Pending {
	resolve: (value: unknown) => void;
	reject: (err: Error) => void;
	timer: ReturnType<typeof setTimeout>;
}

/** The version this client implements. Servers negotiate down from theirs. */
const PROTOCOL_VERSION = "2025-06-18";

export class McpClient {
	private nextId = 0;
	private readonly pending = new Map<number, Pending>();
	private closed = false;

	constructor(
		private readonly transport: Transport,
		private readonly timeoutMs = 60_000,
	) {
		transport.onMessage((m) => this.handle(m));
	}

	/**
	 * Performs the MCP handshake.
	 *
	 * The `notifications/initialized` follow-up is not optional: a spec-abiding
	 * server rejects every request until it arrives. iterion's own board server
	 * is lenient, which is exactly why omitting it would go unnoticed until the
	 * first third-party server.
	 */
	async initialize(): Promise<void> {
		await this.transport.start();
		await this.request("initialize", {
			protocolVersion: PROTOCOL_VERSION,
			capabilities: {},
			clientInfo: { name: "iterion-pi-extension", version: "1" },
		});
		await this.notify("notifications/initialized");
	}

	async listTools(): Promise<McpTool[]> {
		const tools: McpTool[] = [];
		let cursor: string | undefined;
		// Paginate: a server with many tools returns them in pages, and taking
		// only the first would silently hide the rest.
		do {
			const page = (await this.request("tools/list", cursor ? { cursor } : {})) as
				| { tools?: McpTool[]; nextCursor?: string }
				| undefined;
			for (const t of page?.tools ?? []) tools.push(t);
			cursor = page?.nextCursor;
		} while (cursor);
		return tools;
	}

	async callTool(name: string, args: unknown): Promise<McpCallResult> {
		return ((await this.request("tools/call", { name, arguments: args })) as McpCallResult) ?? {};
	}

	close(): void {
		this.closed = true;
		for (const [, p] of this.pending) {
			clearTimeout(p.timer);
			p.reject(new Error("MCP connection closed"));
		}
		this.pending.clear();
		this.transport.close();
	}

	private async request(method: string, params: unknown): Promise<unknown> {
		if (this.closed) throw new Error(`${method}: client is closed`);
		this.nextId += 1;
		const id = this.nextId;

		const answer = new Promise<unknown>((resolve, reject) => {
			const timer = setTimeout(() => {
				this.pending.delete(id);
				reject(new Error(`${method}: timed out after ${this.timeoutMs}ms`));
			}, this.timeoutMs);
			this.pending.set(id, { resolve, reject, timer });
		});
		// `answer` has no consumer until the send below resolves, but close()
		// can reject it before then — a caller that gave up on a slow server
		// does exactly that. In Node an unhandled rejection is fatal, so
		// without this marker a dead MCP server kills the whole pi process.
		// The returned promise still rejects for the real awaiter.
		answer.catch(() => {});

		try {
			await this.transport.send({ jsonrpc: "2.0", id, method, params });
		} catch (err) {
			// The frame never left. Settle the waiter here or it would sit
			// until the timeout for a failure already known.
			const p = this.pending.get(id);
			if (p) {
				clearTimeout(p.timer);
				this.pending.delete(id);
			}
			throw err instanceof Error ? err : new Error(String(err));
		}
		return answer;
	}

	private async notify(method: string, params?: unknown): Promise<void> {
		await this.transport.send({ jsonrpc: "2.0", method, params });
	}

	private handle(message: JsonRpcMessage): void {
		// Server-initiated requests (sampling, roots, elicitation) are not
		// supported: this client advertises no capabilities, so a conforming
		// server never sends one. Ignoring a stray one is safer than answering
		// on the operator's behalf.
		if (typeof message.id !== "number") return;
		const p = this.pending.get(message.id);
		if (!p) return;
		clearTimeout(p.timer);
		this.pending.delete(message.id);
		if (message.error) {
			p.reject(new Error(`${message.error.message} (code ${message.error.code})`));
			return;
		}
		p.resolve(message.result);
	}
}
