import { describe, expect, it } from "vitest";

import type { WireSchemaField } from "@/api/runs";

import {
  coerceFormAnswerToSchema,
  formSpecFromSchema,
} from "./formSpecFromSchema";

const jsonField = (name: string): WireSchemaField => ({
  name,
  type: "json",
  enum_values: undefined,
});

describe("formSpecFromSchema — JSON checkbox auto-detection", () => {
  it("renders a json `*_ids` field as a checkbox over a sibling array of {id,…}", () => {
    // Mirrors the whats-next ask_which_to_process input: the operator
    // is filling `selected_issue_ids` against a sibling `created_issues`
    // collection. The studio should render a checkbox column, not a
    // raw JSON textarea.
    const spec = formSpecFromSchema(
      [jsonField("selected_issue_ids")],
      {
        created_issues: [
          {
            id: "native:abc12345",
            title: "First issue",
            assignee: "feature_dev",
            horizon: "next_action",
          },
          {
            id: "native:def67890",
            title: "Second issue",
            assignee: "doc-align",
            horizon: "short_term",
          },
        ],
      },
    );
    const q = spec.questions[0]!;
    expect(q.kind).toBe("checkbox");
    if (q.kind !== "checkbox") return;
    expect(q.options.map((o) => o.value)).toEqual([
      "native:abc12345",
      "native:def67890",
    ]);
    expect(q.options[0]?.label).toContain("First issue");
    expect(q.options[0]?.description).toBe("feature_dev · next_action");
    // Pre-tick everything so Approve / Submit defaults to
    // "create / dispatch all" without forcing the operator to
    // hand-pick when they're happy with the bot's proposal.
    expect(q.defaultValues).toEqual([
      "native:abc12345",
      "native:def67890",
    ]);
  });

  it("descends into sibling objects and flattens nested arrays into one checkbox column", () => {
    // The whats-next human_review case: the review_input ships a
    // single `roadmap` object that itself carries long_term[],
    // short_term[], and a singular next_action sub-object. The
    // selected_titles field needs to expand into a checkbox spanning
    // every horizon so the operator can drop items at the proposal
    // stage instead of having to issue every roadmap_item and then
    // close the redundant ones.
    const spec = formSpecFromSchema(
      [jsonField("selected_titles")],
      {
        roadmap: {
          long_term: [
            { title: "Strategic A", assignee: "", horizon: "long_term" },
            { title: "Strategic B", assignee: "", horizon: "long_term" },
          ],
          short_term: [
            { title: "Tactical A", assignee: "feature_dev" },
            { title: "Tactical B", assignee: "doc-align" },
          ],
          next_action: { title: "Do this first", assignee: "feature_dev" },
          rationale: "Because.",
        },
      },
    );
    const q = spec.questions[0]!;
    expect(q.kind).toBe("checkbox");
    if (q.kind !== "checkbox") return;
    // Title is used as value because the items have no `id`.
    expect(q.options.map((o) => o.value)).toEqual([
      "Strategic A",
      "Strategic B",
      "Tactical A",
      "Tactical B",
      "Do this first",
    ]);
    // Approve = create-all defaults to every horizon ticked.
    expect(q.defaultValues).toEqual(q.options.map((o) => o.value));
  });

  it("leaves arbitrary json fields on the free-text path when no sibling collection is in scope", () => {
    const spec = formSpecFromSchema(
      [jsonField("config_blob")],
      { config_blob: { foo: 1 } },
    );
    expect(spec.questions[0]?.kind).toBe("free_text");
  });

  it("flat mode survives the FormSpec round-trip", () => {
    const spec = formSpecFromSchema(
      [{ name: "feedback", type: "string", enum_values: undefined }],
      {},
      { mode: "flat" },
    );
    expect(spec.mode).toBe("flat");
  });
});

describe("coerceFormAnswerToSchema — checkbox → json passthrough", () => {
  it("passes a string[] answer through as the array for a json field", () => {
    const fields: WireSchemaField[] = [jsonField("selected_titles")];
    const { answers, errors } = coerceFormAnswerToSchema(fields, {
      selected_titles: ["Strategic A", "Tactical B"],
    });
    expect(errors).toEqual({});
    expect(answers.selected_titles).toEqual([
      "Strategic A",
      "Tactical B",
    ]);
  });

  it("does not crash on an empty checkbox selection — empty array round-trips", () => {
    const fields: WireSchemaField[] = [jsonField("selected_titles")];
    const { answers, errors } = coerceFormAnswerToSchema(fields, {
      selected_titles: [],
    });
    expect(errors).toEqual({});
    expect(answers.selected_titles).toEqual([]);
  });
});
