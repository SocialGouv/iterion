import { describe, expect, it } from "vitest";
import { formatToolCall } from "./toolFormatters";

describe("formatToolCall — header detail coverage", () => {
  it("Read surfaces file_path", () => {
    const r = formatToolCall("Read", { file_path: "/tmp/x.go" });
    expect(r.fields).toContainEqual(
      expect.objectContaining({ label: "path", value: "/tmp/x.go" }),
    );
  });

  it("WebFetch surfaces url + prompt", () => {
    const r = formatToolCall("WebFetch", {
      url: "https://example.com/api",
      prompt: "Summarize",
    });
    const labels = r.fields.map((f) => f.label);
    expect(labels).toContain("url");
    expect(labels).toContain("prompt");
  });

  it("WebSearch surfaces query", () => {
    const r = formatToolCall("WebSearch", { query: "iterion docs" });
    expect(r.fields).toContainEqual(
      expect.objectContaining({ label: "query", value: "iterion docs" }),
    );
  });

  it("Task surfaces description + subagent", () => {
    const r = formatToolCall("Task", {
      description: "Audit security",
      subagent_type: "security-reviewer",
    });
    const labels = r.fields.map((f) => f.label);
    expect(labels).toContain("description");
    expect(labels).toContain("agent");
  });

  it("Unknown tool falls back to generic fields", () => {
    const r = formatToolCall("ExoticTool", { foo: "bar", count: 3 });
    expect(r.fields.length).toBeGreaterThan(0);
    expect(r.fields[0]?.label).toBe("foo");
  });

  it("Empty input returns no fields and not-unparsed", () => {
    const r = formatToolCall("Read", null);
    expect(r.fields).toEqual([]);
    expect(r.unparsed).toBe(false);
  });
});

describe("formatToolCall — TodoWrite structured rendering", () => {
  const todos = [
    { content: "Set up project", status: "pending", activeForm: "Setting up project" },
    { content: "Implement core", status: "in_progress", activeForm: "Implementing core" },
    { content: "Write tests", status: "completed", activeForm: "Writing tests" },
  ];

  it("emits count summary in fields", () => {
    const r = formatToolCall("TodoWrite", { todos });
    const todoField = r.fields.find((f) => f.label === "todos");
    expect(todoField?.value).toMatch(/3 total/);
    expect(todoField?.value).toMatch(/1 in progress/);
    expect(todoField?.value).toMatch(/1 done/);
    expect(todoField?.value).toMatch(/1 pending/);
  });

  it("emits structured todos[] for the checklist component", () => {
    const r = formatToolCall("TodoWrite", { todos });
    expect(r.todos).toBeDefined();
    expect(r.todos).toHaveLength(3);
    const out = r.todos ?? [];
    expect(out[0]).toEqual(
      expect.objectContaining({ status: "pending", content: "Set up project" }),
    );
    expect(out[1]).toEqual(
      expect.objectContaining({ status: "in_progress", content: "Implement core" }),
    );
    expect(out[2]).toEqual(
      expect.objectContaining({ status: "completed", content: "Write tests" }),
    );
  });

  it("accepts snake_case todo_write tool name (claw side)", () => {
    const r = formatToolCall("todo_write", { todos });
    expect(r.todos).toHaveLength(3);
  });

  it("normalises legacy 'done' status to 'completed'", () => {
    const r = formatToolCall("TodoWrite", {
      todos: [{ content: "x", status: "done" }],
    });
    expect((r.todos ?? [])[0]?.status).toBe("completed");
  });

  it("skips entries without content", () => {
    const r = formatToolCall("TodoWrite", {
      todos: [
        { content: "", status: "pending" },
        { content: "ok", status: "pending" },
      ],
    });
    expect(r.todos).toHaveLength(1);
    expect((r.todos ?? [])[0]?.content).toBe("ok");
  });

  it("returns no todos[] when payload is malformed", () => {
    const r = formatToolCall("TodoWrite", { todos: "not an array" });
    expect(r.todos).toBeUndefined();
  });
});
