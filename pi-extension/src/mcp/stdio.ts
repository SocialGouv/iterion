/**
 * The stdio transport: the server is a child process, the conversation is
 * newline-delimited JSON on its stdin/stdout.
 *
 * This is the transport most `mcp_server:` blocks in the wild declare, so
 * without it a workflow using MCP had to stay off the pi backend entirely.
 *
 * Two properties of the binding matter and are easy to get wrong:
 *   - stdout carries ONLY protocol frames. Anything a server logs belongs on
 *     stderr, so stderr is surfaced rather than silently swallowed.
 *   - Closing stdin is the specified shutdown. Killing is only the backstop
 *     for a server that ignores it.
 */

import { type ChildProcess, spawn } from "node:child_process";
import type { JsonRpcMessage, Transport } from "./protocol.js";

export class StdioTransport implements Transport {
	private handler: (m: JsonRpcMessage) => void = () => {};
	private child?: ChildProcess;
	private buffer = "";
	private closed = false;
	private onExit?: () => void;

	constructor(
		private readonly command: string,
		private readonly args: string[] = [],
		private readonly env: Record<string, string> = {},
		/** Receives the server's stderr, one line at a time. */
		private readonly log: (line: string) => void = () => {},
	) {}

	onMessage(handler: (m: JsonRpcMessage) => void): void {
		this.handler = handler;
	}

	async start(): Promise<void> {
		const child = spawn(this.command, this.args, {
			// The declared env is an OVERLAY, not a replacement: a server
			// launched without PATH or HOME generally cannot start at all, and
			// every other backend forwards the ambient environment too.
			env: { ...process.env, ...this.env },
			stdio: ["pipe", "pipe", "pipe"],
		});
		this.child = child;

		child.stdout?.setEncoding("utf8");
		child.stdout?.on("data", (chunk: string) => this.absorb(chunk));
		child.stderr?.setEncoding("utf8");
		child.stderr?.on("data", (chunk: string) => {
			for (const line of chunk.split("\n")) {
				if (line.trim() !== "") this.log(line);
			}
		});

		// A child outliving pi would hold a port, a lock, or an API session for
		// the rest of the machine's day. Node does not do this for us.
		this.onExit = () => this.close();
		process.once("exit", this.onExit);

		await new Promise<void>((resolve, reject) => {
			const settleOk = () => {
				child.off("error", settleErr);
				resolve();
			};
			const settleErr = (err: Error) => {
				child.off("spawn", settleOk);
				reject(new Error(`spawn ${this.command}: ${err.message}`));
			};
			child.once("spawn", settleOk);
			child.once("error", settleErr);
		});
	}

	async send(message: JsonRpcMessage): Promise<void> {
		if (this.closed) throw new Error("transport is closed");
		const stdin = this.child?.stdin;
		if (!stdin || stdin.destroyed) throw new Error("MCP server is not running");
		// JSON.stringify escapes newlines, so one frame is always one line.
		const line = `${JSON.stringify(message)}\n`;
		await new Promise<void>((resolve, reject) => {
			stdin.write(line, (err) => (err ? reject(err) : resolve()));
		});
	}

	close(): void {
		if (this.closed) return;
		this.closed = true;
		if (this.onExit) process.off("exit", this.onExit);
		const child = this.child;
		if (!child) return;
		try {
			child.stdin?.end();
		} catch {
			/* already gone */
		}
		// Give the server the moment it is entitled to before SIGTERM, but do
		// not hold the event loop open waiting for it.
		const grace = setTimeout(() => {
			try {
				child.kill();
			} catch {
				/* already gone */
			}
		}, 2_000);
		grace.unref?.();
		child.once("exit", () => clearTimeout(grace));
	}

	private absorb(chunk: string): void {
		this.buffer += chunk;
		for (;;) {
			const nl = this.buffer.indexOf("\n");
			if (nl < 0) break;
			const line = this.buffer.slice(0, nl).trim();
			this.buffer = this.buffer.slice(nl + 1);
			if (line === "") continue;
			try {
				this.handler(JSON.parse(line) as JsonRpcMessage);
			} catch {
				// Not a frame. Servers that print to stdout despite the spec
				// are common enough that this must not be fatal.
				this.log(`non-JSON on stdout: ${line.slice(0, 200)}`);
			}
		}
	}
}
