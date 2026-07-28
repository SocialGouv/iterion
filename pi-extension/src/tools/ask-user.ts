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
}
