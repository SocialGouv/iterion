// src/config.ts
var CONTRACT_VERSION = "1";
function env(name) {
  const v = process.env[name];
  return v === void 0 || v === "" ? void 0 : v;
}
function permissionMode(raw) {
  switch (raw) {
    case "ask":
      return "ask";
    case "deny":
      return "deny";
    default:
      return "off";
  }
}
function loadConfig() {
  const hostContract = env("ITERION_PI_CONTRACT") ?? "";
  const iterRaw = env("ITERION_PI_ITERATION");
  const iter = iterRaw === void 0 ? void 0 : Number.parseInt(iterRaw, 10);
  return {
    // An absent contract means "not driven by iterion" — a human running pi
    // with this extension on their PATH, say. Inert, not broken.
    compatible: hostContract === CONTRACT_VERSION,
    hostContract,
    runId: env("ITERION_PI_RUN_ID"),
    nodeId: env("ITERION_PI_NODE_ID"),
    iter: Number.isFinite(iter) ? iter : void 0,
    permission: permissionMode(env("ITERION_PI_PERMISSION")),
    ctrlEnabled: env("ITERION_PI_CTRL") !== "off"
  };
}

// src/ctrl.ts
var CTRL_VERSION = 1;
var Ctrl = class {
  constructor(identity, timeoutMs = 12e4) {
    this.identity = identity;
    this.timeoutMs = timeoutMs;
  }
  seq = 0;
  /**
   * Send a request and await the host's reply.
   *
   * Returns undefined when the host declines, cancels, times out, or answers
   * something unparseable. Callers MUST treat undefined as "no answer" and
   * apply their own fail-safe — for the permission gate that means blocking,
   * because failing open on a gate is worse than failing a tool call.
   */
  async request(op, data, ctx) {
    if (!ctx?.ui) return void 0;
    const payload = this.envelope(op, data);
    let raw;
    try {
      raw = await ctx.ui.input(JSON.stringify(payload), void 0, { timeout: this.timeoutMs });
    } catch {
      return void 0;
    }
    if (typeof raw !== "string" || raw === "") return void 0;
    try {
      const reply = JSON.parse(raw);
      if (!reply || reply.ok !== true) return void 0;
      return reply.data;
    } catch {
      return void 0;
    }
  }
  /** Send one-way. Never throws; a dropped notice must not fail a tool call. */
  notify(op, data, ctx) {
    if (!ctx?.ui) return;
    try {
      ctx.ui.notify(JSON.stringify(this.envelope(op, data)), "info");
    } catch {
    }
  }
  envelope(op, data) {
    this.seq += 1;
    return {
      __iterion: 1,
      v: CTRL_VERSION,
      op,
      runId: this.identity.runId,
      nodeId: this.identity.nodeId,
      iter: this.identity.iter,
      seq: this.seq,
      data
    };
  }
};

// src/hooks/permission.ts
function installPermissionGate(pi, cfg, ctrl) {
  if (cfg.permission === "off") return;
  pi.on("tool_call", async (event, ctx) => {
    const verdict = await ctrl.request(
      "permission.evaluate",
      { tool: event.toolName, input: event.input },
      ctx
    );
    if (!verdict) {
      return {
        block: true,
        reason: "iterion's permission gate could not be reached, so this call is blocked. This is a fail-closed default, not a judgement about the call itself."
      };
    }
    if (verdict.decision === "allow") return void 0;
    if (verdict.escalated) {
      return { block: true, reason: verdict.reason || "escalated to the operator for approval" };
    }
    const rule = verdict.rule ? ` (rule: ${verdict.rule})` : "";
    return {
      block: true,
      reason: verdict.reason ? `${verdict.reason}${rule}` : `blocked by iterion's permission policy${rule}`
    };
  });
}

// src/index.ts
function index_default(pi) {
  const cfg = loadConfig();
  if (cfg.hostContract === "") return;
  if (!cfg.compatible) {
    const msg = `iterion pi-extension: contract mismatch (host "${cfg.hostContract}", extension "${CONTRACT_VERSION}"). Registering nothing \u2014 the permission gate and every other iterion capability are INACTIVE for this run. Rebuild the extension asset, or pin a matching iterion.`;
    pi.on("session_start", (_e, ctx) => {
      ctx?.ui?.notify(msg, "error");
    });
    return;
  }
  if (!cfg.ctrlEnabled) return;
  const ctrl = new Ctrl({ runId: cfg.runId, nodeId: cfg.nodeId, iter: cfg.iter });
  installPermissionGate(pi, cfg, ctrl);
}
export {
  index_default as default
};
