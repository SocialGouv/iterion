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
