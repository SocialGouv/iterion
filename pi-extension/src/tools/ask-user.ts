/**
 * `ask_user` on pi.
 *
 * pi has no way for an agent to reach a human: it is a headless process with
 * no operator attached, so a workflow declaring `interaction: human` had no
 * surface on a pi node at all.
 *
 * The tool does not answer the question. It reports it to iterion, which
 * suspends the RUN — persisting an interaction the studio renders, and
 * resuming later with the operator's answer. That is the only shape that
 * works: the answer may arrive minutes or hours later, from a different
 * process, so the tool cannot block waiting for it.
 *
 * What the model gets back is therefore not an answer but an explanation:
 * the turn is over, the question is with a human, and the conversation will
 * resume with the reply. Saying that plainly matters — an agent told only
 * "error" would retry the question in a loop.
 */

import { Type } from "typebox";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import type { Ctrl } from "../ctrl.js";
import type { IterionConfig } from "../config.js";

const ASK_USER_PARAMS = Type.Object({
	question: Type.String({ description: "The question to put to the operator. Be specific and self-contained." }),
	options: Type.Optional(
		Type.Array(
			Type.Object({
				id: Type.String({ description: "Stable identifier returned when this option is picked." }),
				label: Type.String({ description: "What the operator sees." }),
			}),
			{ description: "Selectable answers. Omit for a free-text question." },
		),
	),
	allow_free_text: Type.Optional(
		Type.Boolean({ description: "Allow a typed answer alongside the options. Implied when there are no options." }),
	),
});

export function installAskUser(pi: ExtensionAPI, cfg: IterionConfig, ctrl: Ctrl): void {
	if (cfg.interaction === "off") return;

	pi.registerTool({
		name: "ask_user",
		label: "Ask the operator",
		// promptSnippet puts the tool in pi's own "Available tools" section;
		// without it a custom tool is omitted there and the model is markedly
		// less likely to reach for it.
		promptSnippet: "ask_user — put a question to the human operator and pause the run for their answer",
		description:
			"Ask the human operator a question and PAUSE the run until they answer. " +
			"Use it when you genuinely cannot proceed without a decision only they can make — " +
			"an ambiguous requirement, a destructive action, a missing credential. " +
			"The run stops here and resumes with their answer, so do not call it for " +
			"anything you can determine yourself, and ask everything you need in one go.",
		parameters: ASK_USER_PARAMS,
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			const question = typeof params.question === "string" ? params.question : "";
			if (question.trim() === "") {
				return {
					content: [{ type: "text" as const, text: "ask_user needs a non-empty question." }],
					details: undefined,
					isError: true,
				};
			}

			const ack = await ctrl.request<{ escalated?: boolean }>(
				"ask_user",
				{
					question,
					options: params.options,
					allow_free_text: params.allow_free_text,
				},
				ctx,
			);

			if (!ack?.escalated) {
				// iterion did not take the question. Say so rather than
				// pretending the human declined — the agent should decide for
				// itself instead of waiting for an answer that is not coming.
				return {
					content: [
						{
							type: "text" as const,
							text:
								"The question could not be delivered to an operator (iterion did not accept it). " +
								"Nobody is going to answer. Proceed using your own judgement and say what you assumed.",
						},
					],
					details: undefined,
					isError: true,
				};
			}

			return {
				content: [
					{
						type: "text" as const,
						text:
							"Question delivered. The run is now PAUSED awaiting the operator's answer; " +
							"this turn ends here and the conversation resumes with their reply. " +
							"Do not ask again or try to continue.",
					},
				],
				details: undefined,
			};
		},
	});

	if (cfg.interaction === "async") installAsyncQuestions(pi, ctrl);
}

/** Wraps a plain string as a tool result. */
function text(body: string, isError = false) {
	return { content: [{ type: "text" as const, text: body }], details: undefined, isError };
}

/**
 * The non-blocking question pair (ADR-081).
 *
 * `ask_user` costs a full pause: the run stops, the operator answers, the run
 * resumes. That is the right price for a decision that must block, and far too
 * high for a question the agent could have asked an hour before it mattered.
 * The pair separates the two — post early, keep working, sync only when the
 * answer is genuinely on the critical path.
 *
 * Neither tool decides anything: both report to iterion, which owns the
 * interaction store and is the only side that can suspend a run. The wording
 * the model reads back also comes from iterion (`AsyncQuestionPostedText`), so
 * an agent sees the same instructions on pi as on claude_code and claw.
 */
function installAsyncQuestions(pi: ExtensionAPI, ctrl: Ctrl): void {
	pi.registerTool({
		name: "ask_user_async",
		label: "Ask the operator (non-blocking)",
		promptSnippet: "ask_user_async — post a question to the operator WITHOUT stopping; answers arrive later",
		description:
			"Post a question to the human operator and CONTINUE WORKING immediately. " +
			"The answer arrives later in your conversation, tagged with the question id. " +
			"Front-load these: ask as early as you can, so the operator has time to reply " +
			"while you work on everything that does not depend on it. " +
			"For a decision that must block right now, use ask_user instead.",
		parameters: ASK_USER_PARAMS,
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			const question = typeof params.question === "string" ? params.question : "";
			if (question.trim() === "") return text("ask_user_async needs a non-empty question.", true);

			const ack = await ctrl.request<{ interactionId?: string; message?: string }>(
				"ask_user_async",
				{ question, options: params.options, allow_free_text: params.allow_free_text },
				ctx,
			);
			if (!ack?.interactionId) {
				// Say what actually happened. An agent told only "error" posts
				// the question again; one told nobody will answer decides.
				return text(
					"The question could not be posted (iterion did not accept it). Nobody is going to " +
						"answer it, and await_answers will not produce it. Proceed using your own " +
						"judgement and say what you assumed.",
					true,
				);
			}
			return text(`[${ack.interactionId}] ${ack.message ?? "Question posted."}`);
		},
	});

	pi.registerTool({
		name: "await_answers",
		label: "Wait for the operator's answers",
		promptSnippet: "await_answers — the sync point for questions posted with ask_user_async",
		description:
			"The sync point for questions posted with ask_user_async. Call it ONLY when you " +
			"truly cannot proceed without the pending answers. If everything you asked is " +
			"already answered it returns the answers immediately and costs nothing; " +
			"otherwise the run PAUSES until the operator replies.",
		parameters: Type.Object({}),
		async execute(_toolCallId, _params, _signal, _onUpdate, ctx) {
			const result = await ctrl.request<{
				answers?: string;
				escalated?: boolean;
				pending?: { interactionId?: string; question?: string }[];
			}>("await_answers", {}, ctx);

			if (!result) {
				return text(
					"iterion did not answer the sync request, so the state of your posted questions " +
						"is unknown. Do not call this again — continue with what you have and say " +
						"which answers you are missing.",
					true,
				);
			}
			if (result.escalated) {
				const n = result.pending?.length ?? 0;
				return text(
					`The run is now PAUSED waiting on ${n} unanswered question(s). This turn ends ` +
						"here and the conversation resumes with the operator's replies. Do not continue.",
				);
			}
			return text(
				result.answers && result.answers.trim() !== ""
					? `${result.answers}\n(Nothing was pending — no pause was needed. Use these answers and continue.)`
					: "Nothing was pending and no question has been answered yet. Continue.",
			);
		},
	});
}
