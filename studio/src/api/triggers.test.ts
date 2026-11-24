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

  // The server derives the poller from (connection, trigger, poll_query)
  // and refuses a request naming one: a pause or resume built from a polled
  // subscription carries its query and backfill, never its poller.
  it("carries a polled subscription's query and backfill, never its poller", () => {
    const sub: TriggerSubscription = {
      id: "s2",
      bot_id: "triager",
      invocation: "connector",
      match: { sources: ["connector"], kinds: ["jira.issue.polled"] },
      connection_id: "conn-jira",
      trigger_id: "jira.issue.polled",
      poller: "pol_abc",
      poll_query: { jql: "project = OPS" },
      poller_since: 42,
      backfill: 3,
      backfill_done: true,
      enabled: false,
    };
    const input = toSubscriptionInput(sub);
    expect(input).toMatchObject({ poll_query: { jql: "project = OPS" }, backfill: 3 });
    expect(Object.keys(input)).not.toContain("poller");
    expect(Object.keys(input)).not.toContain("poller_since");
    expect(Object.keys(input)).not.toContain("backfill_done");
  });
});
