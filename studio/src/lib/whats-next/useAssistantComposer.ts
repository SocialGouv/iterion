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

import type { FirstClassBot } from "./firstClassBots";
import type { WhatsNextMessage } from "./messages";
import type { UseWhatsNextSession } from "./useWhatsNextSession";

type PendingQuestion = Extract<WhatsNextMessage, { kind: "human-question" }>;

export interface AssistantComposer {
  pendingHumanQuestion: PendingQuestion | undefined;
  pendingIsAskUser: boolean;
  // Structured ask_user options (chips). Empty on a plain chat pause.
  options: AskUserOption[];
  // Nexie's suggested next messages on a chat pause.
  quickReplies: string[];
  // An ask_user pause with options may forbid free text — the chips are
  // then the only input.
  allowFreeText: boolean;
  busyPending: boolean;
  // Answer the pending pause with a literal value (a chip click).
  submitPending: (value: string) => Promise<void>;
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
  decorate?: (text: string) => string;
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
  const pendingAnswerKey = pendingIsAskUser
    ? ASK_USER_RESPONSE_KEY
    : bot.nodeMap[pendingHumanQuestion?.nodeId ?? ""]?.textField ?? "message";

  const options = pendingIsAskUser
    ? askUserOptions(pendingHumanQuestion?.questions)
    : [];
  const allowFreeText = pendingIsAskUser
    ? askUserAllowsFreeText(pendingHumanQuestion?.questions)
    : true;
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

  const onComposerSend = useCallback(
    async (text: string, opts: { skills: string[] }) => {
      const trimmed = text.trim();
      if (trimmed === "") return;
      // The page-context pointer rides on whatever the operator typed,
      // including a pause answer — the assistant's next turn must know
      // where they are, not where they were when the turn started.
      const decorated = decorate ? decorate(trimmed) : trimmed;
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
        const seedVar = bot.seedVar ?? "initial_message";
        await session.launch({
          ...(session.lastVars ?? {}),
          [seedVar]: decorated,
        });
        return;
      }
      // Not closed ⇒ runId is truthy (it's part of the `closed`
      // disjunction above), so the run is live: inject into its inbox.
      await queueMessage(session.runId!, decorated, { skills: opts.skills });
    },
    [decorate, pendingHumanQuestion, submitPending, session, bot.seedVar],
  );

  return {
    pendingHumanQuestion,
    pendingIsAskUser,
    options,
    quickReplies,
    allowFreeText,
    busyPending:
      !!pendingHumanQuestion &&
      session.busyMessageId === pendingHumanQuestion.id,
    submitPending,
    onComposerSend,
  };
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
