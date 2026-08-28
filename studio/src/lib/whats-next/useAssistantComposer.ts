// The assistant's composer routing, shared by its two views: the
// full-width /whats-next route and the shell-level dock.
//
// Lifted verbatim out of WhatsNextView when the dock landed. Duplicating
// it would have been the real bug: a submitted message has FOUR possible
// destinations depending on session state, and a second copy drifting
// from this one means the dock silently answers the wrong thing.
//
//   - answers the pending `chat` pause (Nexie's turn-end question),
//   - answers a pending mid-turn ask_user pause,
//   - queues into the running agent's inbox,
//   - or re-seeds a fresh session (vars[seedVar] = the text).

import { useCallback } from "react";

import { queueMessage } from "@/api/queueMessages";
import {
  askUserAllowsFreeText,
  askUserOptions,
  ASK_USER_RESPONSE_KEY,
  type AskUserOption,
} from "@/lib/askUserOptions";

import {
  botDeclaresReviewer,
  readReviewer,
  reviewerVars,
} from "@/lib/chatDock/assistantPrefs";

import type { FirstClassBot } from "./firstClassBots";
import type { WhatsNextMessage } from "./messages";
import type { UseWhatsNextSession } from "./useWhatsNextSession";

type PendingQuestion = Extract<WhatsNextMessage, { kind: "human-question" }>;

export interface AssistantComposer {
  pendingHumanQuestion: PendingQuestion | undefined;
  pendingIsAskUser: boolean;
  // A manifest-declared boolean approval turn. When present, the regular
  // text composer stands down for the shared approval controls.
  pendingApproval:
    | { approvedField: string; textField?: string }
    | undefined;
  // Structured ask_user options (chips). Empty on a plain chat pause.
  options: AskUserOption[];
  // Nexie's suggested next messages on a chat pause.
  quickReplies: string[];
  // An ask_user pause with options may forbid free text — the chips are
  // then the only input.
  allowFreeText: boolean;
  // Whether the dock's page/attachment decorator will be applied to the next
  // typed message. Option-constrained ask_user answers must stay byte-exact.
  willDecorateMessage: boolean;
  busyPending: boolean;
  // True while a launch/discovery is in flight and there is no run to
  // talk to yet. Surfaces disable the composer on it: sending in that
  // window would seed a SECOND session beside the one about to attach.
  launchPending: boolean;
  // Answer the pending pause with a literal value (a chip click).
  submitPending: (value: string) => Promise<void>;
  // Submit the approval boolean and, for a hybrid turn, its optional text
  // under the exact field names declared by the bot manifest.
  submitApproval: (approved: boolean, text?: string) => Promise<void>;
  // The unified composer submit. `decorate` runs on the trimmed text
  // before it is sent — the dock uses it to prepend the page-context
  // pointer, the route view passes nothing.
  onComposerSend: (text: string, opts: { skills: string[] }) => Promise<void>;
}

export function useAssistantComposer({
  bot,
  session,
  decorate,
}: {
  bot: FirstClassBot;
  session: UseWhatsNextSession;
  decorate?: (text: string) => string | Promise<string>;
}): AssistantComposer {
  const pendingHumanQuestion = session.messages.find(
    (m): m is PendingQuestion =>
      m.kind === "human-question" && m.status === "pending",
  );

  // A pending ask_user pause (mid-turn agent question) answers with a
  // single string under ask_user_response; the chat node's pause
  // answers with {message}. Both flow through the same composer.
  const pendingIsAskUser =
    !!pendingHumanQuestion?.questions &&
    ASK_USER_RESPONSE_KEY in pendingHumanQuestion.questions;
  const pendingNode = bot.nodeMap[pendingHumanQuestion?.nodeId ?? ""];
  const pendingApproval =
    !pendingIsAskUser && pendingNode?.approvedField
      ? {
          approvedField: pendingNode.approvedField,
          ...(pendingNode.textField ? { textField: pendingNode.textField } : {}),
        }
      : undefined;
  const pendingAnswerKey = pendingIsAskUser
    ? ASK_USER_RESPONSE_KEY
    : pendingNode?.textField ?? "message";

  const options = pendingIsAskUser
    ? askUserOptions(pendingHumanQuestion?.questions)
    : [];
  const allowFreeText = pendingIsAskUser
    ? askUserAllowsFreeText(pendingHumanQuestion?.questions)
    : true;
  const willDecorateMessage =
    !!decorate && (!pendingIsAskUser || options.length === 0);
  const quickReplies: string[] = !pendingIsAskUser
    ? readQuickReplies(pendingHumanQuestion?.questions)
    : [];

  // Awaited (not fire-and-forget) so the composer's draft survives a
  // failed submit: AgentChatboxInline only clears the text when onSend
  // resolves, and submitHumanAnswer rethrows on failure.
  const submitPending = useCallback(
    async (value: string) => {
      if (!pendingHumanQuestion) return;
      await session.submitHumanAnswer(pendingHumanQuestion.id, {
        [pendingAnswerKey]: value,
      });
    },
    [pendingHumanQuestion, pendingAnswerKey, session],
  );

  const submitApproval = useCallback(
    async (approved: boolean, text = "") => {
      if (!pendingHumanQuestion || !pendingApproval) return;
      const answer: Record<string, unknown> = {
        [pendingApproval.approvedField]: approved,
      };
      if (pendingApproval.textField) {
        answer[pendingApproval.textField] = text.trim();
      }
      await session.submitHumanAnswer(pendingHumanQuestion.id, answer);
    },
    [pendingApproval, pendingHumanQuestion, session],
  );

  const onComposerSend = useCallback(
    async (text: string, opts: { skills: string[] }) => {
      const trimmed = text.trim();
      if (trimmed === "") return;
      // A typed chat answer and a free-form ask_user question may carry page
      // context. Only ask_user with structured options is a constrained value
      // protocol: an option such as "approve" must remain byte-for-byte.
      const decorated =
        willDecorateMessage && decorate ? await decorate(trimmed) : trimmed;
      if (pendingHumanQuestion) {
        await submitPending(decorated);
        return;
      }
      const status = session.runStatus;
      const closed =
        !session.runId ||
        status === "finished" ||
        status === "failed" ||
        status === "cancelled";
      if (closed) {
        // Startup discovery is still resolving whether a live session
        // exists, or a launch we already started has not returned its
        // runId yet. Seeding now creates a SECOND run beside the one
        // about to attach — two Nexies on one workspace, one of which
        // the operator never sees again.
        //
        // Throwing rather than dropping the text is deliberate:
        // useAsyncAction surfaces it in the composer's error slot and
        // leaves the draft intact, so the operator loses nothing but a
        // moment.
        if (session.status === "launching") {
          throw new Error(
            "A session is still starting — give it a second, then send again.",
          );
        }
        const seedVar = bot.seedVar ?? "initial_message";
        // Cross-review is a per-CONVERSATION decision (priced per turn, for
        // the whole session), so it rides the launch vars — but ONLY for a bot
        // whose manifest declares it. Sending it blind would push a var at
        // bots that have none, and guessing which bots support what is exactly
        // what the manifest registry exists to stop.
        //
        // `lastVars` is spread after, so a re-seed of a closed session keeps
        // the choice made when it was launched rather than today's default.
        await session.launch({
          ...(botDeclaresReviewer(bot) ? reviewerVars(readReviewer()) : {}),
          ...(session.lastVars ?? {}),
          [seedVar]: decorated,
        });
        return;
      }
      // Not closed ⇒ runId is truthy (it's part of the `closed`
      // disjunction above), so the run is live: inject into its inbox.
      await queueMessage(session.runId!, decorated, { skills: opts.skills });
    },
    [
      decorate,
      pendingHumanQuestion,
      submitPending,
      session,
      bot,
      willDecorateMessage,
    ],
  );

  return {
    pendingHumanQuestion,
    pendingIsAskUser,
    pendingApproval,
    options,
    quickReplies,
    allowFreeText,
    willDecorateMessage,
    busyPending:
      !!pendingHumanQuestion &&
      session.busyMessageId === pendingHumanQuestion.id,
    launchPending: session.status === "launching" && !session.runId,
    submitPending,
    submitApproval,
    onComposerSend,
  };
}

/** Build the defensive inline-turn payload with the same manifest routing as
 * the shared composer. This path is normally hidden, but must not silently
 * turn a boolean approval into a string if it is ever reached. */
export function assistantHumanAnswer(
  bot: FirstClassBot,
  message: PendingQuestion,
  outcome: {
    text: string;
    approved?: boolean;
    formAnswer?: Record<string, unknown>;
  },
): Record<string, unknown> {
  if (outcome.formAnswer) return outcome.formAnswer;
  if (message.questions && ASK_USER_RESPONSE_KEY in message.questions) {
    return { [ASK_USER_RESPONSE_KEY]: outcome.text };
  }
  const node = bot.nodeMap[message.nodeId];
  const answer: Record<string, unknown> = {};
  if (node?.approvedField && typeof outcome.approved === "boolean") {
    answer[node.approvedField] = outcome.approved;
  }
  if (node?.textField) answer[node.textField] = outcome.text;
  if (!node?.approvedField && !node?.textField) answer.message = outcome.text;
  return answer;
}

// readQuickReplies lifts Nexie's suggested next messages off the chat
// pause's questions payload (`quick_replies: json` on the turn output,
// mapped into the chat node's input). Tolerates absent / malformed
// payloads — chips are sugar, never load-bearing. A `json`-typed schema
// field can arrive as the literal TEXT of a JSON array (the LLM emits
// the array stringified) — parse that shape too.
export function readQuickReplies(
  questions: Record<string, unknown> | undefined,
): string[] {
  let raw = questions?.quick_replies;
  if (typeof raw === "string" && raw.trim().startsWith("[")) {
    try {
      raw = JSON.parse(raw);
    } catch {
      return [];
    }
  }
  if (!Array.isArray(raw)) return [];
  return raw
    .filter((v): v is string => typeof v === "string" && v.trim().length > 0)
    .slice(0, 4);
}
