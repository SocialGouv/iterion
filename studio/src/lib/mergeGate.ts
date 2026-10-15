// Merge-gate posture, as the server and the gating bots read it.
//
// Mirror of pkg/forge.GateValueDisables: an EXPLICIT `gate_enabled` pin
// enables the gate only for the bots' own truthy words ("true", "1", "yes",
// "on" — trimmed, case-insensitive); every other value, the empty string
// included, leaves the bot silent. An ABSENT pin means the gate is on, since
// nothing is pinned by default.
//
// Every studio site that shows the posture (the Enable panel's radios and its
// read-only line, the repo page's select) reads this one table, so what the
// studio claims and what the bot does cannot diverge.

/** True when an explicit `gate_enabled` value leaves the gating bot silent. */
export function gateValueDisables(value: string): boolean {
  switch (value.trim().toLowerCase()) {
    case "true":
    case "1":
    case "yes":
    case "on":
      return false;
    default:
      return true;
  }
}

/** The posture an integration's launch_vars describe — an absent
 *  `gate_enabled` keeps the gate on. */
export function mergeGateEnabled(launchVars?: Record<string, string> | null): boolean {
  const pinned = launchVars?.gate_enabled;
  if (pinned === undefined) return true;
  return !gateValueDisables(pinned);
}

/** The two words the operator reads for each posture. */
export const GATE_ON_LABEL = "Gate merges";
export const GATE_OFF_LABEL = "Advisory only";

/** Name of the posture an integration's launch_vars describe. */
export function mergeGateLabel(launchVars?: Record<string, string> | null): string {
  return mergeGateEnabled(launchVars) ? GATE_ON_LABEL : GATE_OFF_LABEL;
}
