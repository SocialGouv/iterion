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
 * Shipped today: the permission gate, ask_user (blocking and async), and MCP
 * bridging on all three transports (which is what makes board capabilities and
 * workflow-declared servers reachable).
 * Next: Claude-Code tool aliases.
 * See ADR-085.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { CONTRACT_VERSION, loadConfig } from "./config.js";
import { Ctrl } from "./ctrl.js";
import { installPermissionGate } from "./hooks/permission.js";
import type { McpClient } from "./mcp/client.js";
import { installAskUser } from "./tools/ask-user.js";
import { installMcpServer } from "./tools/mcp-tools.js";

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

	// MCP bridging is async (each server is asked what it offers), so it runs
	// on session_start rather than blocking extension load. A server that is
	// unreachable costs its tools, not the session — the agent then simply
	// does not have them, which is visible in what it does.
	if (cfg.mcpServers.length > 0) {
		const clients: McpClient[] = [];

		pi.on("session_start", async (_event, ctx) => {
			// In parallel and each individually bounded: pi awaits this handler
			// before the first turn, and iterion's own handshake is waiting on
			// that, so the cost of a slow server must not be paid twice — nor
			// multiplied by the number of servers.
			await Promise.all(
				cfg.mcpServers.map(async (server) => {
					try {
						const { client, count } = await installMcpServer(
							pi,
							server,
							(line) => ctrl.notify("log", { level: "warn", message: line }, ctx),
							cfg.mcpConnectTimeoutMs,
						);
						clients.push(client);
						ctrl.notify(
							"log",
							{ level: "info", message: `bridged ${count} tool(s) from ${server.name} (${server.transport})` },
							ctx,
						);
					} catch (err) {
						const msg = err instanceof Error ? err.message : String(err);
						ctrl.notify("log", { level: "warn", message: `MCP server ${server.name} unreachable: ${msg}` }, ctx);
					}
				}),
			);
		});

		// A stdio server is a child process and an http/sse one holds a
		// connection; neither goes away on its own. Without this, every run
		// against a stdio MCP server would leak one process for the life of
		// the machine.
		pi.on("session_shutdown", () => {
			while (clients.length > 0) clients.pop()?.close();
		});
	}
}
