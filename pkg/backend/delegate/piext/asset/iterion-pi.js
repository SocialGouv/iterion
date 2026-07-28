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
function parseMcpServers(raw) {
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(
      (s) => typeof s?.name === "string" && typeof s?.url === "string"
    );
  } catch {
    return [];
  }
}
function loadConfig() {
  const hostContract = env("ITERION_PI_CONTRACT") ?? "";
  const iterationRaw = env("ITERION_PI_ITERATION");
  const iteration = iterationRaw === void 0 ? void 0 : Number.parseInt(iterationRaw, 10);
  return {
    // An absent contract means "not driven by iterion" — a human running pi
    // with this extension on their PATH, say. Inert, not broken.
    compatible: hostContract === CONTRACT_VERSION,
    hostContract,
    runId: env("ITERION_PI_RUN_ID"),
    nodeId: env("ITERION_PI_NODE_ID"),
    iteration: Number.isFinite(iteration) ? iteration : void 0,
    permission: permissionMode(env("ITERION_PI_PERMISSION")),
    interaction: env("ITERION_PI_INTERACTION") === "sync" ? "sync" : "off",
    mcpServers: parseMcpServers(env("ITERION_PI_MCP_SERVERS")),
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
      iteration: this.identity.iteration,
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

// src/tools/ask-user.ts
import { Type } from "typebox";
var ASK_USER_PARAMS = Type.Object({
  question: Type.String({ description: "The question to put to the operator. Be specific and self-contained." }),
  options: Type.Optional(
    Type.Array(
      Type.Object({
        id: Type.String({ description: "Stable identifier returned when this option is picked." }),
        label: Type.String({ description: "What the operator sees." })
      }),
      { description: "Selectable answers. Omit for a free-text question." }
    )
  ),
  allow_free_text: Type.Optional(
    Type.Boolean({ description: "Allow a typed answer alongside the options. Implied when there are no options." })
  )
});
function installAskUser(pi, cfg, ctrl) {
  if (cfg.interaction === "off") return;
  pi.registerTool({
    name: "ask_user",
    label: "Ask the operator",
    // promptSnippet puts the tool in pi's own "Available tools" section;
    // without it a custom tool is omitted there and the model is markedly
    // less likely to reach for it.
    promptSnippet: "ask_user \u2014 put a question to the human operator and pause the run for their answer",
    description: "Ask the human operator a question and PAUSE the run until they answer. Use it when you genuinely cannot proceed without a decision only they can make \u2014 an ambiguous requirement, a destructive action, a missing credential. The run stops here and resumes with their answer, so do not call it for anything you can determine yourself, and ask everything you need in one go.",
    parameters: ASK_USER_PARAMS,
    async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
      const question = typeof params.question === "string" ? params.question : "";
      if (question.trim() === "") {
        return {
          content: [{ type: "text", text: "ask_user needs a non-empty question." }],
          details: void 0,
          isError: true
        };
      }
      const ack = await ctrl.request(
        "ask_user",
        {
          question,
          options: params.options,
          allow_free_text: params.allow_free_text
        },
        ctx
      );
      if (!ack?.escalated) {
        return {
          content: [
            {
              type: "text",
              text: "The question could not be delivered to an operator (iterion did not accept it). Nobody is going to answer. Proceed using your own judgement and say what you assumed."
            }
          ],
          details: void 0,
          isError: true
        };
      }
      return {
        content: [
          {
            type: "text",
            text: "Question delivered. The run is now PAUSED awaiting the operator's answer; this turn ends here and the conversation resumes with their reply. Do not ask again or try to continue."
          }
        ],
        details: void 0
      };
    }
  });
}

// src/tools/mcp-tools.ts
import { Type as Type2 } from "typebox";

// src/mcp/http.ts
var McpHttpClient = class {
  constructor(url, headers = {}, timeoutMs = 6e4) {
    this.url = url;
    this.headers = headers;
    this.timeoutMs = timeoutMs;
  }
  nextId = 0;
  /**
   * MCP requires an `initialize` handshake before anything else. iterion's
   * board server is lenient, but a third-party server is not, and doing it
   * here keeps this client honest against both.
   */
  async initialize() {
    await this.rpc("initialize", {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "iterion-pi-extension", version: "1" }
    });
  }
  async listTools() {
    const res = await this.rpc("tools/list", {});
    return res?.tools ?? [];
  }
  async callTool(name, args) {
    return await this.rpc("tools/call", { name, arguments: args }) ?? {};
  }
  async rpc(method, params) {
    this.nextId += 1;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    try {
      const res = await fetch(this.url, {
        method: "POST",
        headers: { "content-type": "application/json", ...this.headers },
        body: JSON.stringify({ jsonrpc: "2.0", id: this.nextId, method, params }),
        signal: controller.signal
      });
      if (!res.ok) {
        throw new Error(`${method}: HTTP ${res.status} ${res.statusText}`);
      }
      const body = await res.json();
      if (body.error) {
        throw new Error(`${method}: ${body.error.message} (code ${body.error.code})`);
      }
      return body.result;
    } finally {
      clearTimeout(timer);
    }
  }
};

// src/tools/mcp-tools.ts
function renderResult(result) {
  const parts = [];
  for (const c of result.content ?? []) {
    if (typeof c.text === "string") parts.push(c.text);
  }
  const text = parts.length > 0 ? parts.join("\n") : JSON.stringify(result);
  return { text, isError: result.isError === true };
}
async function installMcpServer(pi, server) {
  if (!server.url) return 0;
  const client = new McpHttpClient(server.url, server.headers ?? {});
  await client.initialize();
  const tools = await client.listTools();
  for (const tool of tools) {
    pi.registerTool({
      name: tool.name,
      label: tool.name,
      description: tool.description ?? `${tool.name} (via ${server.name})`,
      // The server's JSON Schema is passed through unvalidated on this
      // side: it is authoritative, and re-deriving a TypeBox schema from
      // it would only add a second place for the shape to be wrong.
      parameters: Type2.Unsafe(
        tool.inputSchema ?? { type: "object" }
      ),
      async execute(_toolCallId, params) {
        try {
          const result = await client.callTool(tool.name, params ?? {});
          const { text, isError } = renderResult(result);
          return { content: [{ type: "text", text }], details: void 0, isError };
        } catch (err) {
          const msg = err instanceof Error ? err.message : String(err);
          return {
            content: [{ type: "text", text: `${tool.name} failed: ${msg}` }],
            details: void 0,
            isError: true
          };
        }
      }
    });
  }
  return tools.length;
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
  const ctrl = new Ctrl({ runId: cfg.runId, nodeId: cfg.nodeId, iteration: cfg.iteration });
  installPermissionGate(pi, cfg, ctrl);
  installAskUser(pi, cfg, ctrl);
  if (cfg.mcpServers.length > 0) {
    pi.on("session_start", async (_event, ctx) => {
      for (const server of cfg.mcpServers) {
        try {
          const count = await installMcpServer(pi, server);
          ctrl.notify("log", { level: "info", message: `bridged ${count} tool(s) from ${server.name}` }, ctx);
        } catch (err) {
          const msg = err instanceof Error ? err.message : String(err);
          ctrl.notify("log", { level: "warn", message: `MCP server ${server.name} unreachable: ${msg}` }, ctx);
        }
      }
    });
  }
}
export {
  index_default as default
};
