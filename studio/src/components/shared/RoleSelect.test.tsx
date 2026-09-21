// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ORG_ROLES, TEAM_ROLES, isDemotion } from "@/lib/roles";

import { RoleSelect } from "./RoleSelect";

afterEach(cleanup);

function setup(over: {
  value: string;
  roles: readonly string[];
  confirmChangeFrom?: string;
  answer?: boolean;
}) {
  const onChange = vi.fn();
  const confirm = vi.fn(async () => over.answer ?? true);
  render(
    <RoleSelect
      value={over.value}
      roles={over.roles}
      ariaLabel="Role"
      confirmChangeFrom={over.confirmChangeFrom}
      confirm={confirm}
      onChange={onChange}
    />,
  );
  const select = screen.getByLabelText("Role");
  return { onChange, confirm, select };
}

describe("RoleSelect", () => {
  // The prompt lives here rather than in each page because the
  // super-admin drawer reaches ANY org or team on the platform: a surface
  // that forgot to wrap its own write could hand over ownership of a
  // tenant in one un-prompted click.
  it("confirms an owner handover before writing", async () => {
    const { onChange, confirm, select } = setup({
      value: "admin",
      roles: TEAM_ROLES,
      confirmChangeFrom: "admin",
    });
    fireEvent.change(select, { target: { value: "owner" } });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("owner"));
    expect(confirm).toHaveBeenCalledTimes(1);
  });

  it("confirms a demotion before writing", async () => {
    const { onChange, confirm, select } = setup({
      value: "admin",
      roles: TEAM_ROLES,
      confirmChangeFrom: "admin",
    });
    fireEvent.change(select, { target: { value: "viewer" } });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("viewer"));
    expect(confirm).toHaveBeenCalledTimes(1);
  });

  it("does not write when the prompt is declined", async () => {
    const { onChange, confirm, select } = setup({
      value: "owner",
      roles: TEAM_ROLES,
      confirmChangeFrom: "owner",
      answer: false,
    });
    fireEvent.change(select, { target: { value: "member" } });
    await waitFor(() => expect(confirm).toHaveBeenCalledTimes(1));
    expect(onChange).not.toHaveBeenCalled();
  });

  // A prompt on every change is a prompt nobody reads. A routine promotion
  // takes nothing away and must go straight through.
  it("lets a routine promotion through without a prompt", async () => {
    const { onChange, confirm, select } = setup({
      value: "member",
      roles: TEAM_ROLES,
      confirmChangeFrom: "member",
    });
    fireEvent.change(select, { target: { value: "admin" } });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("admin"));
    expect(confirm).not.toHaveBeenCalled();
  });

  // The org ladder is shorter and has no config_editor; the rule has to
  // read the ladder it was handed, not the team one.
  it("uses the ladder it was given", async () => {
    const { onChange, confirm, select } = setup({
      value: "admin",
      roles: ORG_ROLES,
      confirmChangeFrom: "admin",
    });
    fireEvent.change(select, { target: { value: "member" } });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("member"));
    expect(confirm).toHaveBeenCalledTimes(1);
  });

  // Choosing a role to GRANT takes nothing from anyone yet, so a plain
  // change on that path goes straight through.
  it("does not prompt for an ordinary grant", async () => {
    const { onChange, confirm, select } = setup({ value: "member", roles: TEAM_ROLES });
    fireEvent.change(select, { target: { value: "viewer" } });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("viewer"));
    expect(confirm).not.toHaveBeenCalled();
  });

  // …but the `owner` half applies on the grant path too. `handlePutTeamMember`
  // accepts any valid role from a team admin, so installing an owner is
  // reachable WITHOUT ever passing through the change path that prompts.
  it("prompts before GRANTING owner, where no current role exists", async () => {
    const { onChange, confirm, select } = setup({ value: "member", roles: TEAM_ROLES });
    fireEvent.change(select, { target: { value: "owner" } });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("owner"));
    expect(confirm).toHaveBeenCalledTimes(1);
  });

  it("does not grant owner when the prompt is declined", async () => {
    const { onChange, confirm, select } = setup({
      value: "member",
      roles: TEAM_ROLES,
      answer: false,
    });
    fireEvent.change(select, { target: { value: "owner" } });
    await waitFor(() => expect(confirm).toHaveBeenCalledTimes(1));
    expect(onChange).not.toHaveBeenCalled();
  });

  // config_editor is ORTHOGONAL to the ladder server-side (rank 0, ADR-078):
  // it grants exactly one capability and nothing else. Moving off it revokes
  // that capability, so the move is a revocation however the ladder's index
  // reads — and `indexOf` alone would call it a promotion.
  it("prompts when moving OFF config_editor, which the ladder index calls a promotion", async () => {
    const { onChange, confirm, select } = setup({
      value: "config_editor",
      roles: TEAM_ROLES,
      confirmChangeFrom: "config_editor",
    });
    fireEvent.change(select, { target: { value: "viewer" } });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("viewer"));
    expect(confirm).toHaveBeenCalledTimes(1);
  });

  // The other direction of the same asymmetry, tested on the predicate
  // because a role the ladder does not carry has no <option> to select:
  // `indexOf` returns -1, and `-1 < n` is true while `n < -1` is false — so
  // moving OUT of an unknown role would never have prompted.
  it("treats a role the ladder does not carry as a demotion in BOTH directions", () => {
    expect(isDemotion("viewer", "mystery", TEAM_ROLES)).toBe(true);
    expect(isDemotion("mystery", "viewer", TEAM_ROLES)).toBe(true);
    expect(isDemotion("mystery", "admin", TEAM_ROLES)).toBe(true);
    // And config_editor, which IS in the ladder but is not a rung on it.
    expect(isDemotion("config_editor", "admin", TEAM_ROLES)).toBe(true);
    expect(isDemotion("admin", "config_editor", TEAM_ROLES)).toBe(true);
    // The ordinary ladder still reads as an ordinary ladder.
    expect(isDemotion("member", "admin", TEAM_ROLES)).toBe(false);
    expect(isDemotion("admin", "member", TEAM_ROLES)).toBe(true);
  });
});
