// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import type { Confirmer } from "@/hooks/useConfirm";
import { ORG_ROLES } from "@/lib/roles";

import { AddExistingMemberPanel, type MemberCandidate } from "./AddExistingMemberPanel";

afterEach(cleanup);

const ALICE: MemberCandidate = { user_id: "u-alice", email: "alice@example.org" };
const BOB: MemberCandidate = { user_id: "u-bob", email: "bob@example.org" };

// A host that resolves candidates from the SERVER: its list is a function of
// the query, so clearing the query empties it. That is the real shape of the
// super-admin org roster, and the state in which a picker can end up naming
// nobody while still holding a selection.
function ServerBackedHost({
  onAdd,
  confirm,
}: {
  onAdd: (u: string, r: string) => Promise<void>;
  confirm?: Confirmer;
}) {
  const [query, setQuery] = useState("");
  const q = query.trim().toLowerCase();
  const candidates =
    q === "" ? [] : [ALICE, BOB].filter((c) => (c.email ?? "").startsWith(q));
  return (
    <AddExistingMemberPanel
      title="Add an account that already exists"
      candidates={candidates}
      roles={ORG_ROLES}
      defaultRole="member"
      emptyMessage="Type an email prefix to find an account."
      addLabel="Add to org"
      onQueryChange={setQuery}
      confirm={confirm}
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

  // The guard belongs to the WRITE. Wired to the role dropdown it fired on a
  // gesture that wrote nothing — and since the panel deliberately keeps the
  // role after a successful add, the SECOND owner grant went out with the
  // prompt already spent. Two grants, two prompts.
  it("prompts before EVERY owner grant, not once per dropdown change", async () => {
    const onAdd = vi.fn(async () => {});
    const confirm = vi.fn(async () => true);
    render(<ServerBackedHost onAdd={onAdd} confirm={confirm} />);

    const pick = async (email: string, id: string) => {
      fireEvent.click(screen.getByLabelText("Account"));
      fireEvent.change(screen.getByPlaceholderText("Search by email…"), {
        target: { value: email.slice(0, 3) },
      });
      fireEvent.mouseDown(await screen.findByText(email));
      await waitFor(() =>
        expect(screen.getByLabelText("Account").textContent).toContain(email),
      );
      fireEvent.click(screen.getByRole("button", { name: "Add to org" }));
      await waitFor(() => expect(onAdd).toHaveBeenCalledWith(id, "owner"));
    };

    // Choose owner ONCE; the form keeps it.
    fireEvent.change(screen.getByLabelText("Role"), { target: { value: "owner" } });
    expect(confirm).not.toHaveBeenCalled(); // the dropdown writes nothing

    await pick("alice@example.org", "u-alice");
    expect(confirm).toHaveBeenCalledTimes(1);

    await pick("bob@example.org", "u-bob");
    expect(confirm).toHaveBeenCalledTimes(2);
  });

  it("does not write when the ownership prompt is declined", async () => {
    const onAdd = vi.fn(async () => {});
    const confirm = vi.fn(async () => false);
    render(<ServerBackedHost onAdd={onAdd} confirm={confirm} />);

    fireEvent.change(screen.getByLabelText("Role"), { target: { value: "owner" } });
    fireEvent.click(screen.getByLabelText("Account"));
    fireEvent.change(screen.getByPlaceholderText("Search by email…"), {
      target: { value: "ali" },
    });
    fireEvent.mouseDown(await screen.findByText("alice@example.org"));
    fireEvent.click(screen.getByRole("button", { name: "Add to org" }));

    await waitFor(() => expect(confirm).toHaveBeenCalledTimes(1));
    expect(onAdd).not.toHaveBeenCalled();
  });

  // A prompt on every grant is a prompt nobody reads.
  it("does not prompt for an ordinary grant", async () => {
    const onAdd = vi.fn(async () => {});
    const confirm = vi.fn(async () => true);
    render(<ServerBackedHost onAdd={onAdd} confirm={confirm} />);

    fireEvent.click(screen.getByLabelText("Account"));
    fireEvent.change(screen.getByPlaceholderText("Search by email…"), {
      target: { value: "ali" },
    });
    fireEvent.mouseDown(await screen.findByText("alice@example.org"));
    fireEvent.click(screen.getByRole("button", { name: "Add to org" }));

    await waitFor(() => expect(onAdd).toHaveBeenCalledWith("u-alice", "member"));
    expect(confirm).not.toHaveBeenCalled();
  });

  it("names nobody, and refuses to submit, when nothing is picked", () => {
    render(<ServerBackedHost onAdd={async () => {}} />);
    expect(screen.getByLabelText("Account").textContent).toContain("Search by email…");
    expect(
      screen.getByRole("button", { name: "Add to org" }).hasAttribute("disabled"),
    ).toBe(true);
  });
});
