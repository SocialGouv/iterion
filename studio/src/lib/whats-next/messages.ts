// WhatsNextMessage describes one entry in the chat transcript. The
// transcript is a flat ordered list; events from the run lifecycle
// fold into messages via the generic runChat module
// (`@/lib/runChat/messagesFromEvents`) parameterised by a whats-next
// resolver in `messagesFromEvents.ts` (this directory).
//
// whats-next v2: Nexie is a single conversational agent, so the
// transcript is entirely made of the SHARED envelope shapes — banners
// while Nexie works, assistant-text narration, the chat human turns,
// operator messages, session-closed markers. The v1 typed cards
// (roadmap / survey / issues-summary / dispatch-candidates /
// triage-summary / plan-handed-off) fell with the form state machine.
//
// The shared envelope types live in `@/lib/runChat/types` and are
// re-exported here for backward compatibility with the WhatsNext
// components that import them under these names.

export type {
  BannerStatus,
  BannerProgress,
  BannerMessage,
  HumanQuestionMessage,
  QuickActionKind,
  SessionClosedMessage,
  UserMessage,
  UserMessageStatus,
  AssistantTextMessage,
} from "@/lib/runChat/types";

import type {
  AssistantTextMessage as _AssistantTextMessage,
  BannerMessage as _BannerMessage,
  HumanQuestionMessage as _HumanQuestionMessage,
  SessionClosedMessage as _SessionClosedMessage,
  UserMessage as _UserMessage,
} from "@/lib/runChat/types";

export type WhatsNextMessage =
  | _BannerMessage
  | _HumanQuestionMessage
  | _SessionClosedMessage
  | _UserMessage
  | _AssistantTextMessage;
