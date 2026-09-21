// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ORG_ROLES } from "@/lib/roles";

import { AddExistingMemberPanel, type MemberCandidate } from "./AddExistingMemberPanel";

afterEach(cleanup);

const ALICE: MemberCandidate = { user_id: "u-alice", email: "alice@example.org" };

// A host that resolves candidates from the SERVER: its list is a function of
// the query, so clearing the query empties it. That is the real shape of the
// super-admin org roster, and the state in which a picker can end up naming
// nobody while still holding a selection.
function ServerBackedHost({ onAdd }: { onAdd: (u: string, r: string) => Promise<void> }) {
  const [query, setQuery] = useState("");
  const candidates = query.trim() === "" ? [] : [ALICE];
  return (
    <AddExistingMemberPanel
      title="Add an account that already exists"
      candidates={candidates}
      roles={ORG_ROLES}
      defaultRole="member"
      emptyMessage="Type an email prefix to find an account."
      addLabel="Add to org"
      onQueryChange={setQuery}
      onAdd={onAdd}
    />
  );
}

describe("AddExistingMemberPanel with a server-backed candidate list", () => {
  // The regression this guards: committing a pick closes the Combobox, which
  // clears the search, which empties a server-backed list — so the option the
  // operator just chose leaves `options`. Before the fix the trigger fell back
  // to the PLACEHOLDER while "Add to org" stayed enabled: a panel that names
  // nobody, one click from placing an account the operator cannot see.
  it("keeps naming the picked account after the search that found it is cleared", async () => {
    const onAdd = vi.fn(async () => {});
    render(<ServerBackedHost onAdd={onAdd} />);

    fireEvent.click(screen.getByLabelText("Account"));
    fireEvent.change(screen.getByPlaceholderText("Search by email…"), {
      target: { value: "ali" },
    });
    fireEvent.mouseDown(await screen.findByText("alice@example.org"));

    // The pick committed, and the host's query has been cleared back to "".
    await waitFor(() =>
      expect(screen.getByLabelText("Account").textContent).toContain(
        "alice@example.org",
      ),
    );

    // And the button it arms still acts on that account.
    fireEvent.click(screen.getByRole("button", { name: "Add to org" }));
    await waitFor(() => expect(onAdd).toHaveBeenCalledWith("u-alice", "member"));
  });

  it("names nobody, and refuses to submit, when nothing is picked", () => {
    render(<ServerBackedHost onAdd={async () => {}} />);
    expect(screen.getByLabelText("Account").textContent).toContain("Search by email…");
    expect(
      screen.getByRole("button", { name: "Add to org" }).hasAttribute("disabled"),
    ).toBe(true);
  });
});
