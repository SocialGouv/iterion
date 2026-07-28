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
 *   - Secret VALUES never travel here. Tokens ride inside a server descriptor
 *     that is read once, so a generic environment dump cannot log them.
 */

/** Bumped when the Go↔extension contract changes incompatibly. */
export const CONTRACT_VERSION = "1";

export type PermissionMode = "off" | "ask" | "deny";
export type InteractionMode = "off" | "sync";

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
		interaction: env("ITERION_PI_INTERACTION") === "sync" ? "sync" : "off",
		ctrlEnabled: env("ITERION_PI_CTRL") !== "off",
	};
}
