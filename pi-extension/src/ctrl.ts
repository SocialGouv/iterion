/**
 * The control channel between this extension and the iterion process driving
 * pi over RPC.
 *
 * pi's `RpcExtensionUIRequest` union is closed — an extension cannot add a
 * method — but two of its members are enough to build a full bidirectional
 * channel with no new wire protocol, no listener, and no port:
 *
 *   - `ctx.ui.input(title)` is request/response: it surfaces to the RPC client
 *     and resolves with whatever the client answers.
 *   - `ctx.ui.notify(message)` is fire-and-forget, for one-way sends.
 *
 * Every payload carries `__iterion: 1`. That is not decoration: the UI channel
 * is SHARED with any other extension the operator has installed, so the host
 * must be able to tell an iterion control message from a genuine dialog
 * request. Without the marker a hostile or merely buggy extension could
 * fabricate one.
 */

import type { ExtensionContext } from "@earendil-works/pi-coding-agent";

/** Wire version. The host refuses a payload it does not recognise. */
export const CTRL_VERSION = 1;

export interface CtrlEnvelope {
	__iterion: number;
	v: number;
	op: string;
	runId?: string;
	nodeId?: string;
	iteration?: number;
	seq: number;
	data?: unknown;
}

export interface CtrlReply<T = unknown> {
	v: number;
	ok: boolean;
	data?: T;
	error?: string;
}

export interface CtrlIdentity {
	runId?: string;
	nodeId?: string;
	iteration?: number;
}

export class Ctrl {
	private seq = 0;

	constructor(
		private readonly identity: CtrlIdentity,
		/** Milliseconds a request may wait before the caller's fail-safe applies. */
		private readonly timeoutMs = 120_000,
	) {}

	/**
	 * Send a request and await the host's reply.
	 *
	 * Returns undefined when the host declines, cancels, times out, or answers
	 * something unparseable. Callers MUST treat undefined as "no answer" and
	 * apply their own fail-safe — for the permission gate that means blocking,
	 * because failing open on a gate is worse than failing a tool call.
	 */
	async request<T = unknown>(op: string, data?: unknown, ctx?: ExtensionContext): Promise<T | undefined> {
		if (!ctx?.ui) return undefined;
		const payload = this.envelope(op, data);
		let raw: string | undefined;
		try {
			raw = await ctx.ui.input(JSON.stringify(payload), undefined, { timeout: this.timeoutMs });
		} catch {
			return undefined;
		}
		if (typeof raw !== "string" || raw === "") return undefined;
		try {
			const reply = JSON.parse(raw) as CtrlReply<T>;
			if (!reply || reply.ok !== true) return undefined;
			return reply.data as T;
		} catch {
			return undefined;
		}
	}

	/** Send one-way. Never throws; a dropped notice must not fail a tool call. */
	notify(op: string, data?: unknown, ctx?: ExtensionContext): void {
		if (!ctx?.ui) return;
		try {
			ctx.ui.notify(JSON.stringify(this.envelope(op, data)), "info");
		} catch {
			/* ignore */
		}
	}

	private envelope(op: string, data?: unknown): CtrlEnvelope {
		this.seq += 1;
		return {
			__iterion: 1,
			v: CTRL_VERSION,
			op,
			runId: this.identity.runId,
			nodeId: this.identity.nodeId,
			iteration: this.identity.iteration,
			seq: this.seq,
			data,
		};
	}
}
