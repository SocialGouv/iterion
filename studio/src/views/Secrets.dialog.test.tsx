// @vitest-environment jsdom
//
// The add / rotate dialog of the local secrets view. It shares its name field
// with the team secrets tab (SecretNameField); these tests hold the dialog to
// the same two promises through its own wiring: no error under a name nobody
// has had a chance to fill, and inputs a browser does not take for a login.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { LocalSecretView } from "@/api/secrets";

const listLocalSecrets = vi.fn<() => Promise<LocalSecretView[]>>();
vi.mock("@/api/secrets", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/secrets")>();
  return { ...actual, listLocalSecrets: () => listLocalSecrets() };
});

import Secrets from "./Secrets";

const SECRETS: LocalSecretView[] = [
  { id: "s-1", name: "OVH_TOKEN", scope: "global", last4: "9f21", created_at: "2026-10-01T10:00:00Z" },
];

beforeEach(() => {
  listLocalSecrets.mockReset();
  listLocalSecrets.mockResolvedValue(SECRETS);
});
afterEach(cleanup);

async function mountSecrets() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={qc}>
      <Secrets />
    </QueryClientProvider>,
  );
  await screen.findByRole("table", { name: "Local secrets" });
}

describe("local secrets — add dialog", () => {
  it("shows the required-name error only once the empty field was left", async () => {
    await mountSecrets();
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    const dialog = screen.getByRole("dialog", { name: "Add secret" });
    const name = screen.getByPlaceholderText("GITHUB_TOKEN") as HTMLInputElement;
    const create = screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;

    expect(dialog.textContent).not.toContain("name required");
    expect(create.disabled).toBe(true);

    fireEvent.blur(name);
    expect(dialog.textContent).toContain("name required");
    expect(create.disabled).toBe(true);
  });

  it("keeps the browser's saved login out of the name and the value", async () => {
    await mountSecrets();
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(screen.getByPlaceholderText("GITHUB_TOKEN").getAttribute("autocomplete")).toBe("off");
    const value = screen.getByPlaceholderText(/never shown again/) as HTMLInputElement;
    expect(value.type).toBe("password");
    // Not "off": a browser ignores it on a password input.
    expect(value.getAttribute("autocomplete")).toBe("new-password");
  });
});

describe("local secrets — the add dialog's submit", () => {
  // The value is filled first, so the name alone decides: with an empty
  // value the button is disabled whatever the name holds.
  it("stays disabled on an invalid name and opens on a valid one", async () => {
    await mountSecrets();
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    const name = screen.getByPlaceholderText("GITHUB_TOKEN") as HTMLInputElement;
    const value = screen.getByPlaceholderText(/never shown again/) as HTMLInputElement;
    const create = screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;
    fireEvent.change(value, { target: { value: "s3cr3t-value" } });
    expect(create.disabled).toBe(true);
    fireEvent.change(name, { target: { value: "1BAD" } });
    expect(create.disabled).toBe(true);
    fireEvent.change(name, { target: { value: "GOOD_NAME" } });
    expect(create.disabled).toBe(false);
  });
});

describe("local secrets — rotate dialog", () => {
  it("keeps the browser's saved login out of the new value", async () => {
    await mountSecrets();
    fireEvent.click(screen.getByRole("button", { name: "Rotate OVH_TOKEN (global)" }));
    screen.getByRole("dialog", { name: "Rotate OVH_TOKEN" });
    const value = screen.getByPlaceholderText(/never shown again/) as HTMLInputElement;
    expect(value.type).toBe("password");
    expect(value.getAttribute("autocomplete")).toBe("new-password");
  });
});
