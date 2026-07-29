/**
 * Scripted pi provider for credential-free smoke tests.
 *
 * Registers a provider named `mock` whose single model replays a canned
 * response instead of calling an LLM. That lets the Go side drive the REAL
 * `pi` binary end to end — argv acceptance, event-stream shape, usage and
 * cost accounting, stop reasons — with no API key, no network, and no cost.
 *
 * The value being guarded is narrow but load-bearing: pisdk/ is a port of
 * pi's wire types, and the only way to know the port still matches is to
 * decode a stream pi actually produced. A hand-written fixture cannot tell
 * you that pi renamed a field.
 *
 * Usage (see pi_smoke_test.go):
 *   ITERION_PI_MOCK_TEXT='hello' \
 *     pi --mode json -e <this file> --model mock/scripted
 *
 * Env knobs, all optional:
 *   ITERION_PI_MOCK_TEXT        text of the assistant reply (default "ok")
 *   ITERION_PI_MOCK_STOP        stopReason: stop | error | aborted (default stop)
 *   ITERION_PI_MOCK_ERROR       errorMessage, when STOP is error
 *   ITERION_PI_MOCK_STATUS      upstream HTTP status, emitted as
 *                               diagnostics[0].error.code (the precise
 *                               failure signal the Go classifier prefers)
 *   ITERION_PI_MOCK_COST        usage.cost.total in USD (default 0)
 *   ITERION_PI_MOCK_IN/_OUT     input/output token counts (default 11/7)
 *   ITERION_PI_MOCK_REASONING   reasoning tokens (a subset of output)
 *   ITERION_PI_MOCK_TOOL_ARGS   JSON arguments for that call (default {command:"ls"})
 *   ITERION_PI_MOCK_TOOL        name of a tool to call instead of replying
 *                               (exercises the permission gate); the reply
 *                               text is emitted on the SECOND call
 *   ITERION_PI_MOCK_TOOLS       JSON array of {name, arguments} replayed one
 *                               per turn before the text reply — for a flow
 *                               that only exists across several calls, such as
 *                               ask_user_async followed by await_answers.
 *                               Takes precedence over ITERION_PI_MOCK_TOOL.
 */

import {
	type AssistantMessage,
	type AssistantMessageEventStream,
	type Context,
	createAssistantMessageEventStream,
	type Model,
	type SimpleStreamOptions,
	type StopReason,
} from "@earendil-works/pi-ai";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const num = (key: string, fallback: number): number => {
	const raw = process.env[key];
	if (raw === undefined || raw === "") return fallback;
	const parsed = Number(raw);
	return Number.isFinite(parsed) ? parsed : fallback;
};

const state = { calls: 0 };

interface ScriptedCall {
	name: string;
	arguments?: Record<string, unknown>;
}

/**
 * The tool calls to replay, in order, one per turn.
 *
 * A single ITERION_PI_MOCK_TOOL is the one-element case; the array form exists
 * because some flows only appear across several turns and cannot be observed
 * from one call.
 */
function scriptedCalls(): ScriptedCall[] {
	const many = process.env.ITERION_PI_MOCK_TOOLS;
	if (many) {
		try {
			const parsed = JSON.parse(many);
			if (Array.isArray(parsed)) return parsed as ScriptedCall[];
		} catch {
			/* fall through to the single-tool form */
		}
	}
	const one = process.env.ITERION_PI_MOCK_TOOL;
	if (!one) return [];
	let args: Record<string, unknown> = { command: "ls" };
	if (process.env.ITERION_PI_MOCK_TOOL_ARGS) {
		try {
			args = JSON.parse(process.env.ITERION_PI_MOCK_TOOL_ARGS);
		} catch {
			/* keep the default */
		}
	}
	return [{ name: one, arguments: args }];
}

export default function (pi: ExtensionAPI) {
	pi.registerProvider("mock", {
		name: "Mock (scripted)",
		baseUrl: "http://127.0.0.1:1/never-dialled",
		apiKey: "not-used",
		api: "openai-completions",
		models: [
			{
				id: "scripted",
				name: "Scripted",
				contextWindow: 128000,
				maxTokens: 16384,
				cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			},
		],
		streamSimple(model: Model<any>, _context: Context, _options?: SimpleStreamOptions): AssistantMessageEventStream {
			const stream = createAssistantMessageEventStream();
			const text = process.env.ITERION_PI_MOCK_TEXT ?? "ok";
			const stopReason = (process.env.ITERION_PI_MOCK_STOP ?? "stop") as StopReason;
			const reasoning = process.env.ITERION_PI_MOCK_REASONING;

			const input = num("ITERION_PI_MOCK_IN", 11);
			const output = num("ITERION_PI_MOCK_OUT", 7);
			const total = num("ITERION_PI_MOCK_COST", 0);

			// Tool-call mode: each scripted call takes a turn, then the reply.
			// This is what exercises a permission gate — a text-only reply
			// never reaches one.
			const script = scriptedCalls();
			if (state.calls < script.length) {
				const scripted = script[state.calls];
				const toolArgs = scripted.arguments ?? {};
				state.calls += 1;
				const call: AssistantMessage = {
					role: "assistant",
					content: [
						{ type: "toolCall", id: `mock-call-${state.calls}`, name: scripted.name, arguments: toolArgs },
					],
					api: model.api,
					provider: model.provider,
					model: model.id,
					// Unique per turn: the Go parser de-dupes usage by
					// (timestamp, responseId), so reusing one id across a
					// multi-call script would silently under-count.
					responseId: `mock-response-tool-${state.calls}`,
					usage: {
						input, output, cacheRead: 0, cacheWrite: 0,
						totalTokens: input + output,
						cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total },
					},
					stopReason: "toolUse",
					timestamp: 1_800_000_000_000,
				};
				stream.push({ type: "start", message: call });
				stream.push({ type: "done", message: call, reason: "toolUse" });
				return stream;
			}

			const message: AssistantMessage = {
				role: "assistant",
				content: stopReason === "stop" ? [{ type: "text", text }] : [],
				api: model.api,
				provider: model.provider,
				model: model.id,
				responseId: "mock-response-1",
				usage: {
					input,
					output,
					cacheRead: 0,
					cacheWrite: 0,
					...(reasoning !== undefined && reasoning !== "" ? { reasoning: Number(reasoning) } : {}),
					totalTokens: input + output,
					cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total },
				},
				stopReason,
				...(process.env.ITERION_PI_MOCK_ERROR ? { errorMessage: process.env.ITERION_PI_MOCK_ERROR } : {}),
				...(process.env.ITERION_PI_MOCK_STATUS
					? {
							diagnostics: [
								{
									type: "provider_error",
									timestamp: 1_800_000_000_000,
									error: {
										name: "ProviderError",
										message: process.env.ITERION_PI_MOCK_ERROR ?? "scripted failure",
										code: Number(process.env.ITERION_PI_MOCK_STATUS),
									},
								},
							],
						}
					: {}),
				timestamp: 1_800_000_000_000,
			};

			// pi's stream contract is "never throw": a failure is a terminal
			// message carrying stopReason error/aborted, not a rejection.
			if (stopReason === "stop") {
				stream.push({ type: "start", message });
				stream.push({ type: "text_start", contentIndex: 0, partial: message });
				stream.push({ type: "text_delta", contentIndex: 0, delta: text, partial: message });
				stream.push({ type: "text_end", contentIndex: 0, content: { type: "text", text }, partial: message });
			}
			stream.push({ type: "done", message, reason: stopReason });
			return stream;
		},
	});
}
