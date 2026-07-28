/**
 * The legacy HTTP+SSE transport (MCP 2024-11-05).
 *
 * Superseded by streamable HTTP, and still what a good share of deployed
 * servers speak — a workflow declaring `transport: sse` means this one.
 *
 * The shape is asymmetric: a long-lived GET carries every server→client frame,
 * while client→server frames are POSTed to a URL the server announces on that
 * stream as its first `endpoint` event. Nothing can be sent before it arrives.
 */

import type { JsonRpcMessage, Transport } from "./protocol.js";
import { readSseFrames } from "./sse-frames.js";

export class SseTransport implements Transport {
	private handler: (m: JsonRpcMessage) => void = () => {};
	private endpoint?: string;
	private readonly controller = new AbortController();
	private closed = false;

	constructor(
		private readonly url: string,
		private readonly headers: Record<string, string> = {},
		private readonly timeoutMs = 60_000,
	) {}

	onMessage(handler: (m: JsonRpcMessage) => void): void {
		this.handler = handler;
	}

	/** Opens the event stream and resolves once the POST endpoint is known. */
	async start(): Promise<void> {
		const res = await fetch(this.url, {
			method: "GET",
			headers: { accept: "text/event-stream", ...this.headers },
			signal: this.controller.signal,
		});
		if (!res.ok || !res.body) {
			throw new Error(`SSE connect: HTTP ${res.status} ${res.statusText}`);
		}

		let ready!: () => void;
		let failed!: (e: Error) => void;
		const announced = new Promise<void>((resolve, reject) => {
			ready = resolve;
			failed = reject;
		});
		const timer = setTimeout(
			() => failed(new Error(`SSE connect: no endpoint announced within ${this.timeoutMs}ms`)),
			this.timeoutMs,
		);

		// The stream outlives start(): it is how every response arrives.
		void this.pump(res.body, ready, failed).finally(() => clearTimeout(timer));
		await announced;
	}

	async send(message: JsonRpcMessage): Promise<void> {
		if (this.closed) throw new Error("transport is closed");
		if (!this.endpoint) throw new Error("SSE endpoint not announced yet");
		const controller = new AbortController();
		const timer = setTimeout(() => controller.abort(), this.timeoutMs);
		try {
			// Responses come back on the event stream, never in this body.
			const res = await fetch(this.endpoint, {
				method: "POST",
				headers: { "content-type": "application/json", ...this.headers },
				body: JSON.stringify(message),
				signal: controller.signal,
			});
			if (!res.ok) throw new Error(`HTTP ${res.status} ${res.statusText}`);
		} finally {
			clearTimeout(timer);
		}
	}

	close(): void {
		this.closed = true;
		this.controller.abort();
	}

	private async pump(body: ReadableStream<Uint8Array>, ready: () => void, failed: (e: Error) => void): Promise<void> {
		try {
			for await (const frame of readSseFrames(body)) {
				if (frame.event === "endpoint") {
					// Servers announce either an absolute URL or a path; both
					// resolve against the stream's own URL.
					this.endpoint = new URL(frame.data, this.url).toString();
					ready();
					continue;
				}
				if (frame.event !== "message") continue;
				try {
					this.handler(JSON.parse(frame.data) as JsonRpcMessage);
				} catch {
					/* a malformed frame costs its own message, not the stream */
				}
			}
			// The stream ended. If it ended before announcing, start() is still
			// waiting and must be told rather than left to time out.
			failed(new Error("SSE stream closed before announcing an endpoint"));
		} catch (err) {
			if (!this.closed) failed(err instanceof Error ? err : new Error(String(err)));
		}
	}
}
