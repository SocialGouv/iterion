// @vitest-environment jsdom
// The typed client's two URL channels (#3439): `params` fills the path's
// {name} templates and nothing else; `query` is serialised as URL search
// params. A value the client cannot place is refused, never dropped.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { apiDelete, apiGet } from "./typed";

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

describe("typed client — query channel", () => {
  it("sends the query option as URL search params", async () => {
    await apiGet("/api/teams/{id}/connections", {
      params: { id: "t1" },
      query: { connector: "rss" },
    });
    expect(sent).toEqual(["/api/teams/t1/connections?connector=rss"]);
  });

  it("encodes query values and path values each by their own rules", async () => {
    await apiGet("/api/teams/{id}/connections", {
      params: { id: "t 1/x" },
      query: { connector: "a&b c" },
    });
    expect(sent).toEqual(["/api/teams/t%201%2Fx/connections?connector=a%26b+c"]);
  });

  it("omits an undefined query value, and the '?' with it", async () => {
    await apiGet("/api/teams/{id}/connections", {
      params: { id: "t1" },
      query: { connector: undefined },
    });
    expect(sent).toEqual(["/api/teams/t1/connections"]);
  });

  it("serialises numbers as the operation declares them", async () => {
    await apiDelete("/api/admin/bots/{slug}", {
      params: { slug: "steward" },
      query: { version: 3, bot_id: "b1" },
    });
    expect(sent).toEqual(["/api/admin/bots/steward?version=3&bot_id=b1"]);
  });

  it("refuses a query value that is not a scalar", async () => {
    const query = { connector: ["rss", "jira"] } as unknown as { connector: string };
    await expect(
      apiGet("/api/teams/{id}/connections", { params: { id: "t1" }, query }),
    ).rejects.toThrow('query key "connector" of /api/teams/{id}/connections is not a string');
    expect(sent).toEqual([]);
  });
});

describe("typed client — params fill path templates only", () => {
  it("refuses a params key that names no path template, and sends nothing", async () => {
    // A spread escapes the excess-property check: the run-time guard is
    // what catches it.
    const extra: Record<string, string> = { connector: "rss" };
    await expect(
      apiGet("/api/teams/{id}/connections", { params: { id: "t1", ...extra } }),
    ).rejects.toThrow(
      'params key "connector" names no {connector} template in /api/teams/{id}/connections',
    );
    expect(sent).toEqual([]);
  });

  it("refuses a path template that params leave empty, and sends nothing", async () => {
    const params = {} as { id: string };
    await expect(apiGet("/api/teams/{id}/connections", { params })).rejects.toThrow(
      "path template {id} of /api/teams/{id}/connections has no value in params",
    );
    expect(sent).toEqual([]);
  });
});

// Compile-time witnesses: `tsc -b` fails on an unused @ts-expect-error, so
// each of these stays a type error or the typecheck goes red. They also run,
// so each is awaited to its refusal or its (ignored) response.
describe("typed client — the spec types both channels", () => {
  it("rejects a query key in params, a path key in query, and a query the operation does not declare", async () => {
    await expect(
      apiGet("/api/teams/{id}/connections", {
        // @ts-expect-error connector is a query parameter, not a path template
        params: { id: "t1", connector: "rss" },
      }),
    ).rejects.toThrow('params key "connector"');

    await apiGet("/api/teams/{id}/connections", {
      params: { id: "t1" },
      // @ts-expect-error id is a path template, not a query parameter
      query: { id: "t1" },
    });

    await apiGet("/api/orgs/{id}", {
      params: { id: "o1" },
      // @ts-expect-error GET /api/orgs/{id} declares no query parameter
      query: { connector: "rss" },
    });
  });
});
