/**
 * iterion's permission gate on pi.
 *
 * pi ships no permission system at all — its README says so outright — so
 * without this a `pi` node runs every tool call unconditionally, and a
 * workflow's `permission: ask|deny` block is silently inert on it.
 *
 * The decision is NOT made here. Each call is forwarded to iterion, which
 * evaluates it against `permission.Policy` — the same object that drives
 * claude_code's PreToolUse hook and claw's tool gate. That indirection is the
 * point: the three backends must reach identical decisions for the same
 * workflow, and a second implementation of the rule parser and glob matcher in
 * TypeScript would drift from the Go one the first time either changed. One
 * JSON line each way on an already-open pipe is nothing against an LLM turn.
 */

import type { ExtensionAPI, ExtensionContext, ToolCallEvent } from "@earendil-works/pi-coding-agent";
import type { Ctrl } from "../ctrl.js";
import type { IterionConfig } from "../config.js";

/** What iterion answers for a tool call. */
interface PermissionVerdict {
	/** "allow" | "deny" — `ask` is resolved host-side before replying. */
	decision?: string;
	/** Human-readable cause, shown to the model in the block message. */
	reason?: string;
	/** The rule that matched, for the operator's benefit. */
	rule?: string;
	/** True when the host escalated to a human and is pausing the run. */
	escalated?: boolean;
}

export function installPermissionGate(pi: ExtensionAPI, cfg: IterionConfig, ctrl: Ctrl): void {
	// `off` is the default and the common case: register nothing at all, so a
	// node that does not use the gate pays no per-tool-call round-trip.
	if (cfg.permission === "off") return;

	pi.on("tool_call", async (event: ToolCallEvent, ctx: ExtensionContext) => {
		const verdict = await ctrl.request<PermissionVerdict>(
			"permission.evaluate",
			{ tool: event.toolName, input: event.input },
			ctx,
		);

		// FAIL CLOSED. No answer means the host is gone, timed out, or declined
		// — and a permission gate that fails open is worse than a failed tool
		// call, because the whole point is to bound what an injected or
		// hypnotised agent can do when nobody is watching.
		if (!verdict) {
			return {
				block: true,
				reason:
					"iterion's permission gate could not be reached, so this call is blocked. " +
					"This is a fail-closed default, not a judgement about the call itself.",
			};
		}

		if (verdict.decision === "allow") return undefined;

		// The host escalated to a human: it is already aborting the turn to
		// pause the run, so blocking here just keeps the tool from running in
		// the window before that lands.
		if (verdict.escalated) {
			return { block: true, reason: verdict.reason || "escalated to the operator for approval" };
		}

		const rule = verdict.rule ? ` (rule: ${verdict.rule})` : "";
		return {
			block: true,
			reason: verdict.reason
				? `${verdict.reason}${rule}`
				: `blocked by iterion's permission policy${rule}`,
		};
	});
}
