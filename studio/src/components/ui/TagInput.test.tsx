// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { TagInput } from "./TagInput";

afterEach(cleanup);

// `maxTagLength` cuts a LABEL to length; a `literalValues` list is exempt.
// The two halves are one claim: the cap exists so a label stays readable,
// and a literal (a permission rule) means its exact characters — cutting one
// hands back a different, narrower rule with nothing said, the same class as
// folding its case or splitting it on a comma.

function commit(draft: string) {
  const input = screen.getByRole("textbox");
  fireEvent.change(input, { target: { value: draft } });
  fireEvent.keyDown(input, { key: "Enter" });
}

describe("TagInput and maxTagLength", () => {
  it("cuts a label-shaped tag to the cap", () => {
    const onChange = vi.fn();
    render(<TagInput value={[]} onChange={onChange} maxTagLength={8} />);
    commit("abcdefghij");
    expect(onChange).toHaveBeenCalledWith(["abcdefgh"]);
  });

  it("commits a literal value whole, cap or no cap", () => {
    const onChange = vi.fn();
    render(<TagInput value={[]} onChange={onChange} maxTagLength={8} literalValues />);
    commit("Bash(git diff:*)");
    expect(onChange).toHaveBeenCalledWith(["Bash(git diff:*)"]);
  });
});
