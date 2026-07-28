/**
 * @iterion/pi-extension — gives the `pi` backend the capabilities pi does not
 * ship natively.
 *
 * pi is deliberately small: no permission system, no MCP client, no
 * subagents, no todo tool. That is a reasonable product decision for pi and a
 * problem for iterion, whose workflows declare `permission:` blocks, board
 * `capabilities:` and `mcp_server:` and expect them to mean something on every
 * backend. This extension closes that gap from OUTSIDE pi, through its public
 * ExtensionAPI — no fork, no patch, nothing to re-merge on pi's weekly release.
 *
 * Loaded by the Go backend as `pi -e <path>`. That vector is deliberate: pi's
 * project-trust gate silently ignores `.pi/extensions/` in non-interactive
 * modes, so an extension shipped that way would never load and never say so.
 * CLI `-e` paths bypass trust resolution entirely.
 *
 * Shipped today: the permission gate and ask_user.
 * Next: async questions, board tools, Claude-Code tool aliases, an MCP
 * client. See ADR-085.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { CONTRACT_VERSION, loadConfig } from "./config.js";
import { Ctrl } from "./ctrl.js";
import { installPermissionGate } from "./hooks/permission.js";
import { installAskUser } from "./tools/ask-user.js";

export default function (pi: ExtensionAPI): void {
	const cfg = loadConfig();

	// Not driven by iterion at all (a human running pi with this on their
	// PATH). Stay completely inert rather than half-wiring a control channel
	// that has nobody on the other end.
	if (cfg.hostContract === "") return;

	// Driven by an iterion that speaks a different contract. Fail LOUD and
	// INERT: registering half a permission gate would be worse than
	// registering none, because the operator would believe they had one.
	if (!cfg.compatible) {
		const msg =
			`iterion pi-extension: contract mismatch (host "${cfg.hostContract}", ` +
			`extension "${CONTRACT_VERSION}"). Registering nothing — the permission gate ` +
			`and every other iterion capability are INACTIVE for this run. ` +
			`Rebuild the extension asset, or pin a matching iterion.`;
		pi.on("session_start", (_e, ctx) => {
			ctx?.ui?.notify(msg, "error");
		});
		return;
	}

	if (!cfg.ctrlEnabled) return;

	const ctrl = new Ctrl({ runId: cfg.runId, nodeId: cfg.nodeId, iteration: cfg.iteration });
	installPermissionGate(pi, cfg, ctrl);
	installAskUser(pi, cfg, ctrl);
}
