import { describe, expect, it } from "vitest";

import { createEmptyDocument, defaultPrompt, defaultSchema } from "./defaults";

// A schema's .bot body IS its fields and a prompt's IS its text, so neither
// has a placeholder that would not become content: the save guard
// (pkg/dsl/unparse.Verify) refuses such a declaration BY NAME with a 422 on
// the whole file. A created declaration must therefore never be born empty,
// or "add a prompt, save" blocks the document.
describe("a created declaration is expressible as .bot", () => {
  it("gives a new prompt a body", () => {
    expect(defaultPrompt("prompt_1").body.trim()).not.toBe("");
  });

  it("gives a new schema a field", () => {
    expect(defaultSchema("schema_1").fields.length).toBeGreaterThan(0);
  });

  it("holds for the starter document too", () => {
    const doc = createEmptyDocument();
    for (const p of doc.prompts) expect(p.body.trim()).not.toBe("");
    for (const s of doc.schemas) expect(s.fields.length).toBeGreaterThan(0);
  });
});
