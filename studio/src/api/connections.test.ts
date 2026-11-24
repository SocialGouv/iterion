// @vitest-environment jsdom
// listConnections narrows server-side (#3439): the connector travels as the
// `connector` query parameter pkg/server filters on, not as a path value.
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { listConnections } from "./connections";

let sent: string[] = [];

beforeEach(() => {
  sent = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request) => {
      sent.push(String(input));
      return new Response(JSON.stringify({ connections: [] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

it("asks the server for one connector's connections", async () => {
  await listConnections("t1", "rss");
  expect(sent).toEqual(["/api/teams/t1/connections?connector=rss"]);
});

it("asks for every connection when no connector is named", async () => {
  await listConnections("t1");
  await listConnections("t1", "");
  expect(sent).toEqual(["/api/teams/t1/connections", "/api/teams/t1/connections"]);
});
