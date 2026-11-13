import { describe, expect, it } from "vitest";

import { dslNameError, isDslName, suggestDslName } from "./dslName";

// The studio's name fields hold the .bot lexer's rule: a field that took
// `fix loop` or `user-input` produced a file the save then refused with the
// parser's errors at columns the author never wrote.
describe("dslName", () => {
  it("reads names as the .bot lexer does", () => {
    for (const ok of ["fix_loop", "_x", "a1", "Review", "цикл_правок", "x٣"]) {
      expect(isDslName(ok), ok).toBe(true);
    }
    for (const bad of ["fix loop", "user-input", '"q"', "2nd", "a.b", " a", ""]) {
      expect(isDslName(bad), bad).toBe(false);
    }
  });

  it("says what a name may hold and offers one that works", () => {
    expect(dslNameError("fix loop")).toContain("fix_loop");
    expect(dslNameError("2nd")).toContain("_2nd");
    expect(dslNameError("fix_loop")).toBeNull();
    // Empty is the caller's to word: some fields allow none while typing.
    expect(dslNameError("")).toBeNull();
  });

  it("suggests a spelling the lexer reads", () => {
    for (const name of ["fix loop", "user-input", '"quoted"', "2nd", "цикл правок", ""]) {
      expect(isDslName(suggestDslName(name)), name).toBe(true);
    }
  });
});
