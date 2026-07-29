// A minimal stdio MCP server, for proving the pi extension's stdio transport
// end to end: newline-delimited JSON-RPC on stdin/stdout, nothing else.
//
// It records the methods it saw (and the value of an env override it was given)
// to the file named by ITERION_TEST_MCP_LOG, so the test can assert that the
// server really ran as a child process rather than inferring it from the
// agent's output.

import { appendFileSync } from "node:fs";

const logPath = process.env.ITERION_TEST_MCP_LOG;
const record = (line) => {
	if (logPath) appendFileSync(logPath, `${line}\n`);
};

record(`env:${process.env.ITERION_TEST_MCP_GREETING ?? ""}`);

const send = (msg) => process.stdout.write(`${JSON.stringify(msg)}\n`);

function handle(req) {
	record(`method:${req.method}`);
	// A notification has no id and takes no answer.
	if (req.id === undefined || req.id === null) return;

	switch (req.method) {
		case "initialize":
			send({
				jsonrpc: "2.0",
				id: req.id,
				result: {
					protocolVersion: "2025-06-18",
					capabilities: { tools: {} },
					serverInfo: { name: "mock-stdio", version: "1.0.0" },
				},
			});
			break;
		case "tools/list":
			send({
				jsonrpc: "2.0",
				id: req.id,
				result: {
					tools: [
						{
							name: "mcp__probe__echo",
							description: "Echo a word back",
							inputSchema: {
								type: "object",
								properties: { word: { type: "string" } },
								required: ["word"],
							},
						},
					],
				},
			});
			break;
		case "tools/call": {
			const word = req.params?.arguments?.word ?? "?";
			send({
				jsonrpc: "2.0",
				id: req.id,
				result: { content: [{ type: "text", text: `stdio echoed ${word}` }] },
			});
			break;
		}
		default:
			send({ jsonrpc: "2.0", id: req.id, error: { code: -32601, message: "unknown method" } });
	}
}

let buffer = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
	buffer += chunk;
	for (;;) {
		const nl = buffer.indexOf("\n");
		if (nl < 0) break;
		const line = buffer.slice(0, nl).trim();
		buffer = buffer.slice(nl + 1);
		if (line === "") continue;
		try {
			handle(JSON.parse(line));
		} catch (err) {
			record(`parse-error:${err.message}`);
		}
	}
});

// Closing stdin is the specified shutdown for this transport.
process.stdin.on("end", () => {
	record("stdin-closed");
	process.exit(0);
});
