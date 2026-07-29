/**
 * The streamable-HTTP transport (MCP's current HTTP binding).
 *
 * Every frame is a POST. The server answers either with a single JSON body or
 * with an SSE stream carrying the response — both are legal, and a server that
 * streams is not a server that failed, so both are handled here rather than
 * assumed away. iterion's own board endpoint answers JSON; third-party servers
 * routinely stream.
 */

import type { JsonRpcMessage, Transport } from "./protocol.js";
import { readSseFrames } from "./sse-frames.js";

const PROTOCOL_VERSION = "2025-06-18";

export class HttpTransport implements Transport {
	private handler: (m: JsonRpcMessage) => void = () => {};
	private sessionId?: string;
	private closed = false;
	/** In-flight requests, so close() can actually stop them. */
	private readonly inflight = new Set<AbortController>();

	constructor(
		private readonly url: string,
		private readonly headers: Record<string, string> = {},
		private readonly timeoutMs = 60_000,
	) {}

	// Nothing to establish: the transport is request-driven, and the handshake
	// is the client's first POST.
	async start(): Promise<void> {}

	onMessage(handler: (m: JsonRpcMessage) => void): void {
		this.handler = handler;
	}

	close(): void {
		this.closed = true;
		// A request left running would hold its connection — and, when the
		// caller gave up on a slow server, would keep doing so for the rest of
		// the session.
		for (const c of this.inflight) c.abort();
		this.inflight.clear();
	}

	async send(message: JsonRpcMessage): Promise<void> {
		if (this.closed) throw new Error("transport is closed");
		const controller = new AbortController();
		this.inflight.add(controller);
		const timer = setTimeout(() => controller.abort(), this.timeoutMs);
		try {
			const res = await fetch(this.url, {
				method: "POST",
				headers: {
					"content-type": "application/json",
					// Both are advertised because the server chooses which to
					// use per response.
					accept: "application/json, text/event-stream",
					...(this.sessionId ? { "mcp-session-id": this.sessionId } : {}),
					...(this.sessionId ? { "mcp-protocol-version": PROTOCOL_VERSION } : {}),
					...this.headers,
				},
				body: JSON.stringify(message),
				signal: controller.signal,
			});

			// A server that assigns a session expects it echoed on every
			// subsequent request; dropping it restarts the conversation.
			const assigned = res.headers.get("mcp-session-id");
			if (assigned) this.sessionId = assigned;

			// 202 is the documented answer to a notification: accepted, no body.
			if (res.status === 202 || res.status === 204) return;
			if (!res.ok) {
				throw new Error(`HTTP ${res.status} ${res.statusText}`);
			}

			const contentType = res.headers.get("content-type") ?? "";
			if (contentType.includes("text/event-stream") && res.body) {
				await this.consumeStream(res.body);
				return;
			}

			const text = await res.text();
			if (text.trim() === "") return;
			this.dispatch(JSON.parse(text));
		} finally {
			clearTimeout(timer);
			this.inflight.delete(controller);
		}
	}

	/**
	 * Reads the SSE stream a POST may answer with, stopping at the response.
	 *
	 * The spec says the server SHOULD close the stream once it has replied, but
	 * a server that holds it open would otherwise keep `send` awaiting forever.
	 * Returning at the first response frame makes the caller's progress depend
	 * on the answer, not on the server's stream hygiene. Leaving the loop
	 * cancels the body, so the connection is not held open either.
	 */
	private async consumeStream(body: ReadableStream<Uint8Array>): Promise<void> {
		for await (const frame of readSseFrames(body)) {
			if (frame.event !== "message") continue;
			let parsed: JsonRpcMessage;
			try {
				parsed = JSON.parse(frame.data) as JsonRpcMessage;
			} catch {
				continue;
			}
			this.dispatch(parsed);
			if (parsed.id !== undefined && parsed.id !== null) return;
		}
	}

	private dispatch(parsed: unknown): void {
		// A batch is legal JSON-RPC and some servers use it for tools/list.
		if (Array.isArray(parsed)) {
			for (const m of parsed) this.handler(m as JsonRpcMessage);
			return;
		}
		this.handler(parsed as JsonRpcMessage);
	}
}
