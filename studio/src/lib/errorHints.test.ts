import { describe, expect, it } from "vitest";

import { errorHint, errorMessage, toastError } from "./errorHints";

describe("errorMessage", () => {
  it("unwraps Error, string, ApiError-like object, and falls back", () => {
    expect(errorMessage(new Error("boom"))).toBe("boom");
    expect(errorMessage("raw string")).toBe("raw string");
    expect(errorMessage({ message: "from object" })).toBe("from object");
    // Object without a string message → JSON, not "[object Object]".
    expect(errorMessage({ code: 7 })).toBe('{"code":7}');
    expect(errorMessage(null)).toBe("null");
    expect(errorMessage(undefined)).toBe("undefined");
  });
});

describe("errorHint", () => {
  const cases: Array<{ input: string; title: string }> = [
    { input: "open /etc/x: permission denied", title: "Permission denied" },
    { input: "EACCES: permission denied, open '/x'", title: "Permission denied" },
    { input: "stat /tmp/x: no such file or directory", title: "File not found" },
    { input: "ENOENT: no such file", title: "File not found" },
    { input: "mkdir /x: file exists", title: "Already exists" },
    { input: "open /x: not a directory", title: "Not a folder" },
    { input: "dial tcp 127.0.0.1:7777: connection refused", title: "Can't reach the server" },
    { input: "Get http://x: dial tcp: lookup no such host", title: "Network unreachable" },
    { input: "TypeError: Failed to fetch", title: "Network unreachable" },
    { input: "HTTP 401: unauthorized", title: "Authentication rejected" },
    { input: "401 Bad credentials", title: "Authentication rejected" },
    { input: "HTTP 403: forbidden", title: "Refused or rate-limited" },
    { input: "API rate limit exceeded", title: "Refused or rate-limited" },
    // "command not found" contains "not found" — must resolve to the
    // tool rule, not the generic 404 rule (ordering guard).
    { input: "sh: glab: command not found", title: "Tool not installed" },
    { input: "fork/exec /usr/bin/x: no such file or directory", title: "Tool not installed" },
    { input: "HTTP 404: not found", title: "Not found" },
    { input: "issue not found", title: "Not found" },
    // Git revision-range failures arrive wrapped in an "API error 500"
    // envelope — the git rule must win over the generic 5xx rule
    // (ordering guard).
    {
      input:
        "API error 500: git log: git log -z --reverse --pretty=format:%H exit status 128 (stderr: fatal: Invalid revision range c2390502..07c78ea)",
      title: "Run branch unavailable",
    },
    {
      input:
        "fatal: ambiguous argument 'iterion/run/x': unknown revision or path not in the working tree",
      title: "Run branch unavailable",
    },
    { input: "HTTP 500: internal server error", title: "Server error" },
    { input: "502 Bad Gateway", title: "Server error" },
    { input: "cannot edit binary file", title: "Binary file" },
    { input: "context canceled", title: "Cancelled" },
    { input: "json: cannot unmarshal string into Go value", title: "Unexpected response" },
  ];

  for (const c of cases) {
    it(`maps "${c.input}" → ${c.title}`, () => {
      expect(errorHint(c.input)?.title).toBe(c.title);
    });
  }

  it("returns null when nothing matches", () => {
    expect(errorHint("some completely novel failure")).toBeNull();
    expect(errorHint(new Error("totally unique wording"))).toBeNull();
  });
});

describe("toastError", () => {
  it("prepends context to the friendly title and emits an error toast", () => {
    const calls: Array<{ message: string; type: string }> = [];
    const addToast = (message: string, type: "success" | "error" | "info" | "warning") => {
      calls.push({ message, type });
    };
    toastError(addToast, new Error("HTTP 404: not found"), "Open file failed");
    expect(calls).toEqual([{ message: "Open file failed: Not found", type: "error" }]);
  });

  it("falls back to the raw message when no rule matches", () => {
    const calls: string[] = [];
    const addToast = (message: string) => {
      calls.push(message);
    };
    toastError(addToast, "weird unmatched error", "Install failed");
    expect(calls).toEqual(["Install failed: weird unmatched error"]);
  });
});
