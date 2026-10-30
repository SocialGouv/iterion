// @vitest-environment jsdom
//
// The apply-time detection #1650 (item 3) asks for: /api/parse answers a
// 200 with `diagnostics` null/empty for a buffer that is not a valid
// standalone workflow — an empty text, or a fragment of a bot in several
// files (declarations, no `workflow`). Nothing in that answer says so, and
// the document it carries then fails /api/validate (C006 "no workflow", or
// the C001/C008 of declarations whose referents live in the fragment's
// siblings) with no prior warning. `fragmentParseWarning` is that prior
// warning, read from the parse response alone.
import { describe, expect, it } from "vitest";

import { createEmptyDocument } from "@/lib/defaults";
import { fragmentParseWarning } from "./fragmentParse";

// A document as /api/parse marshals it for a fragment: declarations, no
// workflow. createEmptyDocument carries one, so the fragment removes it.
const fragmentDocument = () => ({ ...createEmptyDocument(), workflows: [] });

describe("fragmentParseWarning", () => {
  it("warns on the fragment case: 200, diagnostics null, declarations but no workflow", () => {
    const warning = fragmentParseWarning({
      document: fragmentDocument(),
      // The Go side omits a nil slice, so the field is absent — and a
      // literal null must read the same.
      diagnostics: null,
    });
    expect(warning).toMatchObject({ kind: "fragment-parse" });
    expect(warning?.message).toMatch(/no workflow/);
  });

  it("warns on the empty-buffer case: nothing declared at all", () => {
    const warning = fragmentParseWarning({
      document: {
        ...createEmptyDocument(),
        prompts: [],
        schemas: [],
        agents: [],
        workflows: [],
      },
      diagnostics: [],
    });
    expect(warning).toMatchObject({ kind: "fragment-parse" });
    expect(warning?.message).toMatch(/declares nothing/);
  });

  it("stays silent on a healthy parse: a workflow is declared", () => {
    expect(
      fragmentParseWarning({ document: createEmptyDocument(), diagnostics: [] }),
    ).toBeNull();
  });

  it("stays silent when the parse reported errors — the existing diagnostics surface them", () => {
    expect(
      fragmentParseWarning({
        document: fragmentDocument(),
        diagnostics: ["line 3: expected declaration"],
      }),
    ).toBeNull();
  });

  it("stays silent on a salvage (bindable false) — the salvage verdict already says it", () => {
    expect(
      fragmentParseWarning({ document: fragmentDocument(), diagnostics: [], bindable: false }),
    ).toBeNull();
  });

  it("stays silent on a read refusal — refuseReadRefusal throws before any apply", () => {
    expect(
      fragmentParseWarning({
        document: fragmentDocument(),
        diagnostics: [],
        read_refusal: { message: "read budget exhausted" },
      }),
    ).toBeNull();
  });

  it("stays silent when the answer carries no document", () => {
    expect(fragmentParseWarning({ diagnostics: [] })).toBeNull();
  });
});
