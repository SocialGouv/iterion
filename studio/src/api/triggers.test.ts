import { describe, expect, it } from "vitest";

import { toSubscriptionInput, type TriggerSubscription } from "./triggers";

describe("toSubscriptionInput", () => {
  // PUT replaces every request field: a toggle built from a connector
  // subscription must carry its binding back, or the save refuses it.
  it("carries a connector subscription's binding back", () => {
    const sub: TriggerSubscription = {
      id: "s1",
      bot_id: "chatbot",
      invocation: "connector",
      match: { sources: ["connector"], kinds: ["chat.message.posted"] },
      connection_id: "conn-chat",
      trigger_id: "chat.message.posted",
      filters: { room: ["ops"] },
      when: "event.prompt != ''",
      var_map: { prompt: "event.prompt" },
      no_live_run: "drop",
      enabled: true,
    };
    const input = toSubscriptionInput(sub);
    expect(input).toMatchObject({
      connection_id: "conn-chat",
      trigger_id: "chat.message.posted",
      filters: { room: ["ops"] },
      when: "event.prompt != ''",
      var_map: { prompt: "event.prompt" },
      no_live_run: "drop",
    });
  });
});
