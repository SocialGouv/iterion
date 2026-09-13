import assert from "node:assert/strict";
import test from "node:test";

import { createMockOpenAI } from "./mock-openai.mjs";

function completionBody(overrides = {}) {
  return {
    model: "demo-fault-model",
    stream: true,
    messages: [{ role: "user", content: "run the demo" }],
    tools: [
      {
        type: "function",
        function: {
          name: "structured_output",
          parameters: { type: "object" },
        },
      },
    ],
    ...overrides,
  };
}

test("mock OpenAI stays failed until explicitly switched to success", async (t) => {
  const mock = createMockOpenAI();
  const origin = await mock.listen();
  t.after(() => mock.close());

  const reset = await fetch(`${origin}/__control/reset`, { method: "POST" });
  assert.equal(reset.status, 200);

  for (let attempt = 1; attempt <= 2; attempt += 1) {
    const response = await fetch(`${origin}/v1/chat/completions`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(completionBody()),
    });
    assert.equal(response.status, 400);
    assert.match(await response.text(), /deterministic demo rejection/);
  }

  const failedState = await fetch(`${origin}/__control/state`).then((r) => r.json());
  assert.deepEqual(failedState, { mode: "fail", completionRequests: 2 });

  const switched = await fetch(`${origin}/__control/mode`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ mode: "succeed" }),
  });
  assert.equal(switched.status, 200);

  const success = await fetch(`${origin}/v1/chat/completions`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(completionBody()),
  });
  assert.equal(success.status, 200);
  assert.match(success.headers.get("content-type") ?? "", /text\/event-stream/);
  const stream = await success.text();
  assert.match(stream, /structured_output/);
  assert.match(stream, /mission_complete/);
  assert.match(stream, /\[DONE\]/);

  const finalState = await fetch(`${origin}/__control/state`).then((r) => r.json());
  assert.deepEqual(finalState, { mode: "succeed", completionRequests: 3 });
});

test("mock OpenAI rejects routes, models and extra tools outside the demo", async (t) => {
  const mock = createMockOpenAI();
  const origin = await mock.listen();
  t.after(() => mock.close());

  assert.equal((await fetch(`${origin}/v1/models`)).status, 404);

  const wrongModel = await fetch(`${origin}/v1/chat/completions`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(completionBody({ model: "another-model" })),
  });
  assert.equal(wrongModel.status, 400);

  const extraTool = await fetch(`${origin}/v1/chat/completions`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(
      completionBody({
        tools: [
          ...completionBody().tools,
          { type: "function", function: { name: "unexpected" } },
        ],
      }),
    ),
  });
  assert.equal(extraTool.status, 400);

  const state = await fetch(`${origin}/__control/state`).then((r) => r.json());
  assert.equal(state.completionRequests, 0);
});
