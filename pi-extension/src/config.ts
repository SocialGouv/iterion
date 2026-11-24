import { readFileSync } from "node:fs";

/**
 * The extension's whole configuration arrives through the environment, set by
 * the Go backend that spawned pi.
 *
 * Two rules hold throughout:
 *
 *   - Every variable is optional. An absent or empty value means the feature is
 *     off, and the extension degrades to a no-op rather than failing the
 *     session. A run must never die because iterion passed one fewer flag than
 *     this build expected.
 *   - Secret VALUES never travel here. The MCP catalog — whose server
 *     descriptors carry the run token in their headers — sits in a private
 *     file the host wrote for THIS node, and the environment carries only its
 *     path: an env VALUE is world-readable for the whole session, because the
 *     container runtime forwards it on the host's own argv (`docker exec --env
 *     K=V`, /proc/<pid>/cmdline mode 444).
 */

/** Bumped when the Go↔extension contract changes incompatibly. */
export const CONTRACT_VERSION = "1";

export type PermissionMode = "off" | "ask" | "deny";

export type McpTransport = "http" | "sse" | "stdio";

/** One MCP server iterion asked the extension to bridge. */
export interface McpServerConfig {
	name: string;
	/** Empty means `http` — the shape iterion's own board server speaks. */
	transport?: McpTransport;
	/** Endpoint for `http` and `sse`. */
	url?: string;
	/** Auth and routing headers. The ONLY place a token appears. */
	headers?: Record<string, string>;
	/** Executable for `stdio`. */
	command?: string;
	args?: string[];
	/** Extra environment for `stdio`, overlaid on the inherited one. */
	env?: Record<string, string>;
}
/**
 * `sync` gives the node the blocking `ask_user`; `async` adds the
 * non-blocking pair (`ask_user_async` / `await_answers`, ADR-081).
 */
export type InteractionMode = "off" | "sync" | "async";

export interface IterionConfig {
	/** True when the host and this build agree on the contract version. */
	compatible: boolean;
	hostContract: string;

	runId?: string;
	nodeId?: string;
	iteration?: number;

	/** Permission gate mode. `off` skips the hook entirely — no round-trip. */
	permission: PermissionMode;

	/** Whether the node may reach a human. `off` registers no ask_user tool. */
	interaction: InteractionMode;

	/** MCP servers to bridge (iterion's board, plus any the workflow declared). */
	mcpServers: McpServerConfig[];

	/**
	 * How long one server gets to connect and list its tools.
	 *
	 * Bounded well under iterion's own 30s RPC handshake, which is blocked on
	 * this: a server that accepts the connection and then goes quiet must cost
	 * its own tools, never the run.
	 */
	mcpConnectTimeoutMs: number;

	/** Whether the control channel is available at all. */
	ctrlEnabled: boolean;
}

function env(name: string): string | undefined {
	const v = process.env[name];
	return v === undefined || v === "" ? undefined : v;
}

function permissionMode(raw: string | undefined): PermissionMode {
	switch (raw) {
		case "ask":
			return "ask";
		case "deny":
			return "deny";
		default:
			return "off";
	}
}

function interactionMode(raw: string | undefined): InteractionMode {
	// Unknown values fall back to `off`: offering a tool that pauses the run
	// to a node with nobody at the other end strands it on a pause that will
	// never be answered.
	return raw === "async" || raw === "sync" ? raw : "off";
}

function positiveInt(raw: string | undefined, fallback: number): number {
	const n = raw === undefined ? Number.NaN : Number.parseInt(raw, 10);
	return Number.isFinite(n) && n > 0 ? n : fallback;
}

function mcpTransport(raw: unknown): McpTransport {
	return raw === "sse" || raw === "stdio" ? raw : "http";
}

/**
 * A server descriptor is usable only if it carries what its transport needs.
 * Registering one that cannot connect would hand the agent tools failing on
 * every call — worse than absent tools, because the model burns turns
 * discovering they do not work.
 */
function usableMcpServer(s: unknown): s is McpServerConfig {
	const c = s as McpServerConfig | undefined;
	if (typeof c?.name !== "string" || c.name === "") return false;
	return mcpTransport(c.transport) === "stdio"
		? typeof c.command === "string" && c.command !== ""
		: typeof c.url === "string" && c.url !== "";
}

/**
 * Parses the MCP server list from the private file the host wrote for THIS
 * node. The engine never puts the list inline: the JSON carries the run token
 * in its headers, and an env VALUE rides the container runtime's own argv on
 * the host — the surface that made the file the only safe shape. A path is
 * not a secret.
 *
 * A malformed, missing or unreadable file yields NO servers rather than
 * throwing: a broken handoff must not take the whole session down with it,
 * and the missing tools are visible in the agent's own behaviour. The write
 * side holds the refusal (the engine refuses a node whose catalog it could
 * not write); the read side holds the degrade.
 */
function parseMcpServersFile(raw: string | undefined): McpServerConfig[] {
	if (!raw) return [];
	try {
		const parsed = JSON.parse(readFileSync(raw, "utf8"));
		if (!Array.isArray(parsed)) return [];
		return parsed.filter(usableMcpServer).map((s) => ({ ...s, transport: mcpTransport(s.transport) }));
	} catch {
		return [];
	}
}

export function loadConfig(): IterionConfig {
	const hostContract = env("ITERION_PI_CONTRACT") ?? "";
	const iterationRaw = env("ITERION_PI_ITERATION");
	const iteration = iterationRaw === undefined ? undefined : Number.parseInt(iterationRaw, 10);

	return {
		// An absent contract means "not driven by iterion" — a human running pi
		// with this extension on their PATH, say. Inert, not broken.
		compatible: hostContract === CONTRACT_VERSION,
		hostContract,
		runId: env("ITERION_PI_RUN_ID"),
		nodeId: env("ITERION_PI_NODE_ID"),
		iteration: Number.isFinite(iteration) ? iteration : undefined,
		permission: permissionMode(env("ITERION_PI_PERMISSION")),
		interaction: interactionMode(env("ITERION_PI_INTERACTION")),
		mcpServers: parseMcpServersFile(env("ITERION_PI_MCP_SERVERS_FILE")),
		mcpConnectTimeoutMs: positiveInt(env("ITERION_PI_MCP_CONNECT_TIMEOUT_MS"), 10_000),
		ctrlEnabled: env("ITERION_PI_CTRL") !== "off",
	};
}
