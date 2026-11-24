// @vitest-environment jsdom
//
// The two dialogs of the team secrets tab — add and rotate. What they owe the
// operator: a name field that reports an error only once it was given a chance
// to be filled, and inputs a browser does not take for a login form.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { GenericSecretView } from "@/api/secrets";

const api = vi.hoisted(() => ({
  listTeamSecrets: vi.fn(),
  listMySecrets: vi.fn(),
  createTeamSecret: vi.fn(),
}));
vi.mock("@/api/secrets", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/secrets")>();
  return { ...actual, ...api };
});
vi.mock("wouter", () => ({ useLocation: () => ["/integrations", vi.fn()] }));

import SecretsTab from "./SecretsTab";

const TEAM: GenericSecretView[] = [
  {
    id: "t-0001",
    name: "FORGE_TOKEN",
    last4: "0001",
    fingerprint: "fp-t-0001",
    created_at: "2026-10-01T10:00:00Z",
    last_used_at: "2026-10-30T10:00:00Z",
  },
];

beforeEach(() => {
  api.listTeamSecrets.mockResolvedValue(TEAM);
  api.listMySecrets.mockResolvedValue([]);
  api.createTeamSecret.mockResolvedValue(undefined);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

async function mountTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={qc}>
      <SecretsTab teamID="t-1" canManage />
    </QueryClientProvider>,
  );
  await screen.findByRole("table", { name: "Team secrets" });
}

async function openAddTeamSecret() {
  await mountTab();
  fireEvent.click(screen.getByRole("button", { name: "Add secret (Team secrets)" }));
  const dialog = screen.getByRole("dialog", { name: "Add team secret" });
  return {
    dialog,
    name: screen.getByPlaceholderText("GITLAB_TOKEN") as HTMLInputElement,
    secret: screen.getByPlaceholderText(/never shown again/) as HTMLInputElement,
    create: screen.getByRole("button", { name: "Create" }) as HTMLButtonElement,
  };
}

const FORMAT_ERROR = /letters, digits and underscore only/;

describe("Add team secret — the name field", () => {
  it("opens without an error under the empty name, which holds the focus", async () => {
    const { dialog, name, create } = await openAddTeamSecret();
    // The focus is where the dialog put it and has not left the field: no
    // blur stands in for an interaction the operator never had.
    expect(document.activeElement).toBe(name);
    expect(dialog.textContent).not.toContain("name required");
    expect(name.getAttribute("aria-invalid")).toBeNull();
    // The gate does not wait for the message: an empty name cannot be sent.
    expect(create.disabled).toBe(true);
  });

  it("says the name is required once the field was left empty", async () => {
    const { dialog, name, create } = await openAddTeamSecret();
    fireEvent.blur(name);
    expect(dialog.textContent).toContain("name required");
    expect(name.getAttribute("aria-invalid")).toBe("true");
    expect(create.disabled).toBe(true);
  });

  it("reports a malformed name as it is typed, without waiting for a blur", async () => {
    const { dialog, name, secret, create } = await openAddTeamSecret();
    fireEvent.change(secret, { target: { value: "s3cr3t-value" } });
    fireEvent.change(name, { target: { value: "1FORGE" } });
    expect(dialog.textContent).toMatch(FORMAT_ERROR);
    expect(create.disabled).toBe(true);
  });

  it("says the name is required again once a typed name was erased", async () => {
    const { dialog, name } = await openAddTeamSecret();
    fireEvent.change(name, { target: { value: "FORGE" } });
    expect(dialog.textContent).not.toContain("name required");
    fireEvent.change(name, { target: { value: "" } });
    expect(dialog.textContent).toContain("name required");
  });

  it("creates the secret once the name is valid and a value is given", async () => {
    const { dialog, name, secret, create } = await openAddTeamSecret();
    fireEvent.change(name, { target: { value: "FORGE_TOKEN_2" } });
    expect(dialog.textContent).not.toMatch(FORMAT_ERROR);
    // A valid name alone does not open the gate: the value is owed too.
    expect(create.disabled).toBe(true);
    fireEvent.change(secret, { target: { value: "s3cr3t-value" } });
    expect(create.disabled).toBe(false);
    fireEvent.click(create);
    await waitFor(() =>
      expect(api.createTeamSecret).toHaveBeenCalledWith("t-1", {
        name: "FORGE_TOKEN_2",
        secret: "s3cr3t-value",
      }),
    );
  });
});

// A text input above a password input is what a browser takes for a login
// form; it then fills the saved username and password, and the dialog would
// store the operator's password as the secret.
describe("team secret dialogs — the browser's saved login", () => {
  it("is kept out of the add dialog", async () => {
    const { name, secret } = await openAddTeamSecret();
    expect(name.getAttribute("autocomplete")).toBe("off");
    expect(secret.type).toBe("password");
    // Not "off": a browser ignores it on a password input.
    expect(secret.getAttribute("autocomplete")).toBe("new-password");
  });

  it("is kept out of the rotate dialog", async () => {
    await mountTab();
    fireEvent.click(screen.getByRole("button", { name: "Rotate FORGE_TOKEN (team)" }));
    screen.getByRole("dialog", { name: "Rotate FORGE_TOKEN" });
    const value = screen.getByPlaceholderText("paste new value") as HTMLInputElement;
    expect(value.type).toBe("password");
    expect(value.getAttribute("autocomplete")).toBe("new-password");
  });
});
