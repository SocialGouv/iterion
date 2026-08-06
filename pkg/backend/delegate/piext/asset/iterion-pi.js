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
function interactionMode(raw) {
  return raw === "async" || raw === "sync" ? raw : "off";
}
function positiveInt(raw, fallback) {
  const n = raw === void 0 ? Number.NaN : Number.parseInt(raw, 10);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}
function mcpTransport(raw) {
  return raw === "sse" || raw === "stdio" ? raw : "http";
}
function usableMcpServer(s) {
  const c = s;
  if (typeof c?.name !== "string" || c.name === "") return false;
  return mcpTransport(c.transport) === "stdio" ? typeof c.command === "string" && c.command !== "" : typeof c.url === "string" && c.url !== "";
}
function parseMcpServers(raw) {
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(usableMcpServer).map((s) => ({ ...s, transport: mcpTransport(s.transport) }));
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
    interaction: interactionMode(env("ITERION_PI_INTERACTION")),
    mcpServers: parseMcpServers(env("ITERION_PI_MCP_SERVERS")),
    mcpConnectTimeoutMs: positiveInt(env("ITERION_PI_MCP_CONNECT_TIMEOUT_MS"), 1e4),
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
  identity;
  timeoutMs;
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
  if (cfg.interaction === "async") installAsyncQuestions(pi, ctrl);
}
function text(body, isError = false) {
  return { content: [{ type: "text", text: body }], details: void 0, isError };
}
function installAsyncQuestions(pi, ctrl) {
  pi.registerTool({
    name: "ask_user_async",
    label: "Ask the operator (non-blocking)",
    promptSnippet: "ask_user_async \u2014 post a question to the operator WITHOUT stopping; answers arrive later",
    description: "Post a question to the human operator and CONTINUE WORKING immediately. The answer arrives later in your conversation, tagged with the question id. Front-load these: ask as early as you can, so the operator has time to reply while you work on everything that does not depend on it. For a decision that must block right now, use ask_user instead.",
    parameters: ASK_USER_PARAMS,
    async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
      const question = typeof params.question === "string" ? params.question : "";
      if (question.trim() === "") return text("ask_user_async needs a non-empty question.", true);
      const ack = await ctrl.request(
        "ask_user_async",
        { question, options: params.options, allow_free_text: params.allow_free_text },
        ctx
      );
      if (!ack?.interactionId) {
        return text(
          "The question could not be posted (iterion did not accept it). Nobody is going to answer it, and await_answers will not produce it. Proceed using your own judgement and say what you assumed.",
          true
        );
      }
      return text(`[${ack.interactionId}] ${ack.message ?? "Question posted."}`);
    }
  });
  pi.registerTool({
    name: "await_answers",
    label: "Wait for the operator's answers",
    promptSnippet: "await_answers \u2014 the sync point for questions posted with ask_user_async",
    description: "The sync point for questions posted with ask_user_async. Call it ONLY when you truly cannot proceed without the pending answers. If everything you asked is already answered it returns the answers immediately and costs nothing; otherwise the run PAUSES until the operator replies.",
    parameters: Type.Object({}),
    async execute(_toolCallId, _params, _signal, _onUpdate, ctx) {
      const result = await ctrl.request("await_answers", {}, ctx);
      if (!result) {
        return text(
          "iterion did not answer the sync request, so the state of your posted questions is unknown. Do not call this again \u2014 continue with what you have and say which answers you are missing.",
          true
        );
      }
      if (result.escalated) {
        const n = result.pending?.length ?? 0;
        return text(
          `The run is now PAUSED waiting on ${n} unanswered question(s). This turn ends here and the conversation resumes with the operator's replies. Do not continue.`
        );
      }
      return text(
        result.answers && result.answers.trim() !== "" ? `${result.answers}
(Nothing was pending \u2014 no pause was needed. Use these answers and continue.)` : "Nothing was pending and no question has been answered yet. Continue."
      );
    }
  });
}

// src/tools/mcp-tools.ts
import { Type as Type2 } from "typebox";

// src/mcp/client.ts
var PROTOCOL_VERSION = "2025-06-18";
var McpClient = class {
  constructor(transport, timeoutMs = 6e4) {
    this.transport = transport;
    this.timeoutMs = timeoutMs;
    transport.onMessage((m) => this.handle(m));
  }
  transport;
  timeoutMs;
  nextId = 0;
  pending = /* @__PURE__ */ new Map();
  closed = false;
  /**
   * Performs the MCP handshake.
   *
   * The `notifications/initialized` follow-up is not optional: a spec-abiding
   * server rejects every request until it arrives. iterion's own board server
   * is lenient, which is exactly why omitting it would go unnoticed until the
   * first third-party server.
   */
  async initialize() {
    await this.transport.start();
    await this.request("initialize", {
      protocolVersion: PROTOCOL_VERSION,
      capabilities: {},
      clientInfo: { name: "iterion-pi-extension", version: "1" }
    });
    await this.notify("notifications/initialized");
  }
  async listTools() {
    const tools = [];
    let cursor;
    do {
      const page = await this.request("tools/list", cursor ? { cursor } : {});
      for (const t of page?.tools ?? []) tools.push(t);
      cursor = page?.nextCursor;
    } while (cursor);
    return tools;
  }
  async callTool(name, args) {
    return await this.request("tools/call", { name, arguments: args }) ?? {};
  }
  close() {
    this.closed = true;
    for (const [, p] of this.pending) {
      clearTimeout(p.timer);
      p.reject(new Error("MCP connection closed"));
    }
    this.pending.clear();
    this.transport.close();
  }
  async request(method, params) {
    if (this.closed) throw new Error(`${method}: client is closed`);
    this.nextId += 1;
    const id = this.nextId;
    const answer = new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`${method}: timed out after ${this.timeoutMs}ms`));
      }, this.timeoutMs);
      this.pending.set(id, { resolve, reject, timer });
    });
    answer.catch(() => {
    });
    try {
      await this.transport.send({ jsonrpc: "2.0", id, method, params });
    } catch (err) {
      const p = this.pending.get(id);
      if (p) {
        clearTimeout(p.timer);
        this.pending.delete(id);
      }
      throw err instanceof Error ? err : new Error(String(err));
    }
    return answer;
  }
  async notify(method, params) {
    await this.transport.send({ jsonrpc: "2.0", method, params });
  }
  handle(message) {
    if (typeof message.id !== "number") return;
    const p = this.pending.get(message.id);
    if (!p) return;
    clearTimeout(p.timer);
    this.pending.delete(message.id);
    if (message.error) {
      p.reject(new Error(`${message.error.message} (code ${message.error.code})`));
      return;
    }
    p.resolve(message.result);
  }
};

// src/mcp/sse-frames.ts
async function* readSseFrames(body) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (; ; ) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      for (; ; ) {
        const boundary = findFrameBoundary(buffer);
        if (boundary < 0) break;
        const raw = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + frameSeparatorLength(buffer, boundary));
        const frame = parseFrame(raw);
        if (frame) yield frame;
      }
    }
  } finally {
    try {
      await reader.cancel();
    } catch {
    }
  }
}
function findFrameBoundary(buffer) {
  const lf = buffer.indexOf("\n\n");
  const crlf = buffer.indexOf("\r\n\r\n");
  if (lf < 0) return crlf;
  if (crlf < 0) return lf;
  return Math.min(lf, crlf);
}
function frameSeparatorLength(buffer, at) {
  return buffer.startsWith("\r\n\r\n", at) ? 4 : 2;
}
function parseFrame(raw) {
  let event = "";
  let id;
  const data = [];
  for (const line of raw.split("\n")) {
    const clean = line.endsWith("\r") ? line.slice(0, -1) : line;
    if (clean === "" || clean.startsWith(":")) continue;
    const colon = clean.indexOf(":");
    const field = colon < 0 ? clean : clean.slice(0, colon);
    let value = colon < 0 ? "" : clean.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") event = value;
    else if (field === "data") data.push(value);
    else if (field === "id") id = value;
  }
  if (data.length === 0) return void 0;
  return { event: event || "message", data: data.join("\n"), id };
}

// src/mcp/http.ts
var PROTOCOL_VERSION2 = "2025-06-18";
var HttpTransport = class {
  constructor(url, headers = {}, timeoutMs = 6e4) {
    this.url = url;
    this.headers = headers;
    this.timeoutMs = timeoutMs;
  }
  url;
  headers;
  timeoutMs;
  handler = () => {
  };
  sessionId;
  closed = false;
  /** In-flight requests, so close() can actually stop them. */
  inflight = /* @__PURE__ */ new Set();
  // Nothing to establish: the transport is request-driven, and the handshake
  // is the client's first POST.
  async start() {
  }
  onMessage(handler) {
    this.handler = handler;
  }
  close() {
    this.closed = true;
    for (const c of this.inflight) c.abort();
    this.inflight.clear();
  }
  async send(message) {
    if (this.closed) throw new Error("transport is closed");
    const controller = new AbortController();
    this.inflight.add(controller);
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    try {
      const res = await fetch(this.url, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          // Both are advertised because the server chooses which to
          // use per response.
          accept: "application/json, text/event-stream",
          ...this.sessionId ? { "mcp-session-id": this.sessionId } : {},
          ...this.sessionId ? { "mcp-protocol-version": PROTOCOL_VERSION2 } : {},
          ...this.headers
        },
        body: JSON.stringify(message),
        signal: controller.signal
      });
      const assigned = res.headers.get("mcp-session-id");
      if (assigned) this.sessionId = assigned;
      if (res.status === 202 || res.status === 204) return;
      if (!res.ok) {
        throw new Error(`HTTP ${res.status} ${res.statusText}`);
      }
      const contentType = res.headers.get("content-type") ?? "";
      if (contentType.includes("text/event-stream") && res.body) {
        await this.consumeStream(res.body);
        return;
      }
      const text2 = await res.text();
      if (text2.trim() === "") return;
      this.dispatch(JSON.parse(text2));
    } finally {
      clearTimeout(timer);
      this.inflight.delete(controller);
    }
  }
  /**
   * Reads the SSE stream a POST may answer with, stopping at the response.
   *
   * The spec says the server SHOULD close the stream once it has replied, but
   * a server that holds it open would otherwise keep `send` awaiting forever.
   * Returning at the first response frame makes the caller's progress depend
   * on the answer, not on the server's stream hygiene. Leaving the loop
   * cancels the body, so the connection is not held open either.
   */
  async consumeStream(body) {
    for await (const frame of readSseFrames(body)) {
      if (frame.event !== "message") continue;
      let parsed;
      try {
        parsed = JSON.parse(frame.data);
      } catch {
        continue;
      }
      this.dispatch(parsed);
      if (parsed.id !== void 0 && parsed.id !== null) return;
    }
  }
  dispatch(parsed) {
    if (Array.isArray(parsed)) {
      for (const m of parsed) this.handler(m);
      return;
    }
    this.handler(parsed);
  }
};

// src/mcp/sse.ts
var SseTransport = class {
  constructor(url, headers = {}, timeoutMs = 6e4) {
    this.url = url;
    this.headers = headers;
    this.timeoutMs = timeoutMs;
  }
  url;
  headers;
  timeoutMs;
  handler = () => {
  };
  endpoint;
  controller = new AbortController();
  closed = false;
  onMessage(handler) {
    this.handler = handler;
  }
  /** Opens the event stream and resolves once the POST endpoint is known. */
  async start() {
    const res = await fetch(this.url, {
      method: "GET",
      headers: { accept: "text/event-stream", ...this.headers },
      signal: this.controller.signal
    });
    if (!res.ok || !res.body) {
      throw new Error(`SSE connect: HTTP ${res.status} ${res.statusText}`);
    }
    let ready;
    let failed;
    const announced = new Promise((resolve, reject) => {
      ready = resolve;
      failed = reject;
    });
    const timer = setTimeout(
      () => failed(new Error(`SSE connect: no endpoint announced within ${this.timeoutMs}ms`)),
      this.timeoutMs
    );
    void this.pump(res.body, ready, failed).finally(() => clearTimeout(timer));
    await announced;
  }
  async send(message) {
    if (this.closed) throw new Error("transport is closed");
    if (!this.endpoint) throw new Error("SSE endpoint not announced yet");
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    try {
      const res = await fetch(this.endpoint, {
        method: "POST",
        headers: { "content-type": "application/json", ...this.headers },
        body: JSON.stringify(message),
        signal: controller.signal
      });
      if (!res.ok) throw new Error(`HTTP ${res.status} ${res.statusText}`);
    } finally {
      clearTimeout(timer);
    }
  }
  close() {
    this.closed = true;
    this.controller.abort();
  }
  async pump(body, ready, failed) {
    try {
      for await (const frame of readSseFrames(body)) {
        if (frame.event === "endpoint") {
          const announced = new URL(frame.data, this.url);
          if (announced.origin !== new URL(this.url).origin) {
            failed(
              new Error(
                `SSE server announced a cross-origin endpoint (${announced.origin}, stream is ${new URL(this.url).origin}) \u2014 refusing to POST credentials there`
              )
            );
            return;
          }
          this.endpoint = announced.toString();
          ready();
          continue;
        }
        if (frame.event !== "message") continue;
        try {
          this.handler(JSON.parse(frame.data));
        } catch {
        }
      }
      failed(new Error("SSE stream closed before announcing an endpoint"));
    } catch (err) {
      if (!this.closed) failed(err instanceof Error ? err : new Error(String(err)));
    }
  }
};

// src/mcp/stdio.ts
import { spawn } from "node:child_process";
var StdioTransport = class {
  constructor(command, args = [], env2 = {}, log = () => {
  }) {
    this.command = command;
    this.args = args;
    this.env = env2;
    this.log = log;
  }
  command;
  args;
  env;
  log;
  handler = () => {
  };
  child;
  buffer = "";
  closed = false;
  onExit;
  onMessage(handler) {
    this.handler = handler;
  }
  async start() {
    const child = spawn(this.command, this.args, {
      // The declared env is an OVERLAY, not a replacement: a server
      // launched without PATH or HOME generally cannot start at all, and
      // every other backend forwards the ambient environment too.
      env: { ...process.env, ...this.env },
      stdio: ["pipe", "pipe", "pipe"]
    });
    this.child = child;
    child.stdin?.on("error", (err) => this.log(`stdin: ${err.message}`));
    child.stdout?.setEncoding("utf8");
    child.stdout?.on("data", (chunk) => this.absorb(chunk));
    child.stderr?.setEncoding("utf8");
    child.stderr?.on("data", (chunk) => {
      for (const line of chunk.split("\n")) {
        if (line.trim() !== "") this.log(line);
      }
    });
    this.onExit = () => this.close();
    process.once("exit", this.onExit);
    await new Promise((resolve, reject) => {
      const settleOk = () => {
        child.off("error", settleErr);
        child.on("error", (err) => this.log(`process: ${err.message}`));
        resolve();
      };
      const settleErr = (err) => {
        child.off("spawn", settleOk);
        reject(new Error(`spawn ${this.command}: ${err.message}`));
      };
      child.once("spawn", settleOk);
      child.once("error", settleErr);
    });
  }
  async send(message) {
    if (this.closed) throw new Error("transport is closed");
    const stdin = this.child?.stdin;
    if (!stdin || stdin.destroyed) throw new Error("MCP server is not running");
    const line = `${JSON.stringify(message)}
`;
    await new Promise((resolve, reject) => {
      stdin.write(line, (err) => err ? reject(err) : resolve());
    });
  }
  close() {
    if (this.closed) return;
    this.closed = true;
    if (this.onExit) process.off("exit", this.onExit);
    const child = this.child;
    if (!child) return;
    try {
      child.stdin?.end();
    } catch {
    }
    const grace = setTimeout(() => {
      try {
        child.kill();
      } catch {
      }
    }, 2e3);
    grace.unref?.();
    child.once("exit", () => clearTimeout(grace));
  }
  absorb(chunk) {
    this.buffer += chunk;
    for (; ; ) {
      const nl = this.buffer.indexOf("\n");
      if (nl < 0) break;
      const line = this.buffer.slice(0, nl).trim();
      this.buffer = this.buffer.slice(nl + 1);
      if (line === "") continue;
      try {
        this.handler(JSON.parse(line));
      } catch {
        this.log(`non-JSON on stdout: ${line.slice(0, 200)}`);
      }
    }
  }
};

// src/tools/mcp-tools.ts
function qualifyToolName(server, tool) {
  const ours = `mcp__${server}__`;
  if (tool.startsWith(ours)) return tool;
  return ours + (tool.startsWith("mcp__") ? tool.slice("mcp__".length) : tool);
}
function renderResult(result) {
  const parts = [];
  for (const c of result.content ?? []) {
    if (typeof c.text === "string") parts.push(c.text);
  }
  const text2 = parts.length > 0 ? parts.join("\n") : JSON.stringify(result);
  return { text: text2, isError: result.isError === true };
}
function makeTransport(server, log) {
  switch (server.transport) {
    case "stdio":
      return new StdioTransport(
        server.command ?? "",
        server.args ?? [],
        server.env ?? {},
        (line) => log(`${server.name}: ${line}`)
      );
    case "sse":
      return new SseTransport(server.url ?? "", server.headers ?? {});
    default:
      return new HttpTransport(server.url ?? "", server.headers ?? {});
  }
}
function withDeadline(work, ms, what) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`${what} timed out after ${ms}ms`)), ms);
    work.then(
      (v) => {
        clearTimeout(timer);
        resolve(v);
      },
      (e) => {
        clearTimeout(timer);
        reject(e);
      }
    );
  });
}
async function installMcpServer(pi, server, log = () => {
}, connectTimeoutMs = 1e4) {
  const client = new McpClient(makeTransport(server, log));
  try {
    await withDeadline(client.initialize(), connectTimeoutMs, `${server.name}: handshake`);
    const tools = await withDeadline(client.listTools(), connectTimeoutMs, `${server.name}: tools/list`);
    for (const tool of tools) {
      const fqn = qualifyToolName(server.name, tool.name);
      pi.registerTool({
        name: fqn,
        label: fqn,
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
            const { text: text2, isError } = renderResult(result);
            return { content: [{ type: "text", text: text2 }], details: void 0, isError };
          } catch (err) {
            const msg = err instanceof Error ? err.message : String(err);
            return {
              content: [{ type: "text", text: `${fqn} failed: ${msg}` }],
              details: void 0,
              isError: true
            };
          }
        }
      });
    }
    return { client, count: tools.length };
  } catch (err) {
    client.close();
    throw err;
  }
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
    const clients = [];
    pi.on("session_start", async (_event, ctx) => {
      await Promise.all(
        cfg.mcpServers.map(async (server) => {
          try {
            const { client, count } = await installMcpServer(
              pi,
              server,
              (line) => ctrl.notify("log", { level: "warn", message: line }, ctx),
              cfg.mcpConnectTimeoutMs
            );
            clients.push(client);
            ctrl.notify(
              "log",
              { level: "info", message: `bridged ${count} tool(s) from ${server.name} (${server.transport})` },
              ctx
            );
          } catch (err) {
            const msg = err instanceof Error ? err.message : String(err);
            ctrl.notify("log", { level: "warn", message: `MCP server ${server.name} unreachable: ${msg}` }, ctx);
          }
        })
      );
    });
    pi.on("session_shutdown", () => {
      while (clients.length > 0) clients.pop()?.close();
    });
  }
}
export {
  index_default as default
};
