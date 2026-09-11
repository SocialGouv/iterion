import http from "node:http";

const DEFAULT_HOST = "127.0.0.1";
const MAX_BODY_BYTES = 2 * 1024 * 1024;
const MODES = new Set(["fail", "succeed"]);

function jsonResponse(res, status, body) {
  const encoded = JSON.stringify(body);
  res.writeHead(status, {
    "content-type": "application/json",
    "content-length": Buffer.byteLength(encoded),
  });
  res.end(encoded);
}

async function readJSON(req) {
  const chunks = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_BODY_BYTES) throw new Error("request body too large");
    chunks.push(chunk);
  }
  const raw = Buffer.concat(chunks).toString("utf8");
  return raw ? JSON.parse(raw) : {};
}

function validStructuredOutputRequest(body) {
  const tools = Array.isArray(body.tools) ? body.tools : [];
  return (
    body.model === "demo-fault-model" &&
    body.stream === true &&
    tools.length === 1 &&
    tools[0]?.type === "function" &&
    tools[0]?.function?.name === "structured_output"
  );
}

function sendSuccessStream(res) {
  const args = JSON.stringify({
    summary: "Le service de démonstration est sain après reprise.",
    mission_complete: true,
  });
  const base = {
    id: "chatcmpl-iterion-demo",
    object: "chat.completion.chunk",
    created: 1,
    model: "demo-fault-model",
  };
  const chunks = [
    {
      ...base,
      choices: [
        {
          index: 0,
          delta: {
            role: "assistant",
            tool_calls: [
              {
                index: 0,
                id: "call-iterion-demo",
                type: "function",
                function: { name: "structured_output", arguments: args },
              },
            ],
          },
          finish_reason: null,
        },
      ],
    },
    {
      ...base,
      choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }],
    },
  ];

  res.writeHead(200, {
    "content-type": "text/event-stream",
    "cache-control": "no-cache",
    connection: "keep-alive",
  });
  for (const chunk of chunks) res.write(`data: ${JSON.stringify(chunk)}\n\n`);
  res.end("data: [DONE]\n\n");
}

export function createMockOpenAI() {
  const state = { mode: "fail", completionRequests: 0 };
  let server;

  const handler = async (req, res) => {
    try {
      const url = new URL(req.url ?? "/", `http://${DEFAULT_HOST}`);

      if (req.method === "POST" && url.pathname === "/__control/reset") {
        state.mode = "fail";
        state.completionRequests = 0;
        jsonResponse(res, 200, state);
        return;
      }
      if (req.method === "POST" && url.pathname === "/__control/mode") {
        const body = await readJSON(req);
        if (!MODES.has(body.mode)) {
          jsonResponse(res, 400, { error: "mode must be fail or succeed" });
          return;
        }
        state.mode = body.mode;
        jsonResponse(res, 200, state);
        return;
      }
      if (req.method === "GET" && url.pathname === "/__control/state") {
        jsonResponse(res, 200, state);
        return;
      }
      if (req.method !== "POST" || url.pathname !== "/v1/chat/completions") {
        jsonResponse(res, 404, { error: "unexpected demo mock route" });
        return;
      }

      const body = await readJSON(req);
      if (!validStructuredOutputRequest(body)) {
        jsonResponse(res, 400, {
          error: "expected demo-fault-model with one structured_output tool",
        });
        return;
      }

      state.completionRequests += 1;
      if (state.mode === "fail") {
        jsonResponse(res, 400, {
          error: {
            message: "deterministic demo rejection",
            type: "invalid_request_error",
            code: "demo_rejection",
          },
        });
        return;
      }
      sendSuccessStream(res);
    } catch (error) {
      jsonResponse(res, 400, {
        error: error instanceof Error ? error.message : String(error),
      });
    }
  };

  return {
    state,
    async listen({ host = DEFAULT_HOST, port = 0 } = {}) {
      if (server) throw new Error("mock OpenAI server is already listening");
      server = http.createServer(handler);
      await new Promise((resolve, reject) => {
        server.once("error", reject);
        server.listen(port, host, resolve);
      });
      const address = server.address();
      if (!address || typeof address === "string") {
        throw new Error("mock OpenAI server did not expose a TCP address");
      }
      return `http://${host}:${address.port}`;
    },
    async close() {
      if (!server) return;
      const current = server;
      server = undefined;
      await new Promise((resolve, reject) =>
        current.close((error) => (error ? reject(error) : resolve())),
      );
    },
  };
}
