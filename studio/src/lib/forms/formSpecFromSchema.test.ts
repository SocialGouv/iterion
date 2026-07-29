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
            assignee: "docs-refresh",
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

  it("parses a sibling array delivered as a JSON string (real agent `json` output)", () => {
    // A real agent's `json`-typed output (e.g. the Bmady PM's `stories`)
    // reaches the form as a JSON-encoded STRING — iterion stores json
    // fields as raw text so `{{input.stories}}` hands the LLM JSON, not
    // "[object Object]". The checkbox detection must parse it; the earlier
    // tests pass a pre-parsed array (the --var path) and so missed this.
    // Regression for the Bmady select_stories gate found by dogfooding.
    const spec = formSpecFromSchema([jsonField("selected_story_ids")], {
      stories: JSON.stringify([
        { id: "S1", title: "--quiet flag prints the bare version" },
        { id: "S2", title: "Default output unchanged" },
      ]),
    });
    const q = spec.questions[0]!;
    expect(q.kind).toBe("checkbox");
    if (q.kind !== "checkbox") return;
    expect(q.options.map((o) => o.value)).toEqual(["S1", "S2"]);
    expect(q.options[0]?.label).toContain("--quiet flag");
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
            { title: "Tactical B", assignee: "docs-refresh" },
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

describe("formSpecFromSchema — file fields", () => {
  const fileField = (name: string): WireSchemaField => ({
    name,
    type: "file",
    enum_values: undefined,
  });

  it("renders a `file` field as a file question", () => {
    const spec = formSpecFromSchema([fileField("music")], {});
    expect(spec.questions).toHaveLength(1);
    expect(spec.questions[0]).toMatchObject({ kind: "file" });
  });

  it("leaves the file question optional", () => {
    // The DSL has no per-field `required` marker, and forcing a file
    // would strand an operator re-answering a gate whose file was
    // already supplied on an earlier pass.
    const spec = formSpecFromSchema([fileField("music")], {});
    expect(spec.questions[0]).toMatchObject({ required: false });
  });

  it("narrows the picker from the field name, and stays silent when unsure", () => {
    const audio = formSpecFromSchema([fileField("soundtrack")], {});
    expect(audio.questions[0]).toMatchObject({ kind: "file", accept: "audio/*" });

    const image = formSpecFromSchema([fileField("screenshot")], {});
    expect(image.questions[0]).toMatchObject({ kind: "file", accept: "image/*" });

    const unknown = formSpecFromSchema([fileField("payload")], {});
    expect(unknown.questions[0]).toMatchObject({ kind: "file" });
    expect(
      (unknown.questions[0] as { accept?: string }).accept,
    ).toBeUndefined();
  });

  it("wraps the staged upload id in an upload envelope on submit", () => {
    const { answers, errors } = coerceFormAnswerToSchema(
      [fileField("music"), { name: "notes", type: "string" }],
      { music: "up_abc123", notes: "use this take" },
    );
    expect(errors).toEqual({});
    expect(answers.music).toEqual({ upload_id: "up_abc123" });
    expect(answers.notes).toBe("use this take");
  });

  it("omits the field entirely when no file was picked", () => {
    // An empty envelope would make the server try to promote "" and
    // fail the whole resume — an operator who simply skipped an
    // optional upload must not be blocked.
    const { answers, errors } = coerceFormAnswerToSchema(
      [fileField("music")],
      { music: "" },
    );
    expect(errors).toEqual({});
    expect("music" in answers).toBe(false);
  });
});
