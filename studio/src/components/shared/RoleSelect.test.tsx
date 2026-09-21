// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ORG_ROLES, TEAM_ROLES } from "@/lib/roles";

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

  // Choosing a role to GRANT takes nothing from anyone yet, so the same
  // component is a plain select when no current role is named.
  it("never prompts when no current role is named", async () => {
    const { onChange, confirm, select } = setup({ value: "member", roles: TEAM_ROLES });
    fireEvent.change(select, { target: { value: "viewer" } });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("viewer"));
    expect(confirm).not.toHaveBeenCalled();
  });
});
