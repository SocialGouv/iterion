// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    registerForgeOAuthApp: vi.fn(),
    getMyGitHubOrgs: vi.fn(async () => []),
  };
});

import { registerForgeOAuthApp } from "@/api/forgeConnections";

import { RegisterOAuthAppForm } from "../integrations/RegisterOAuthAppForm";

const registerMock = vi.mocked(registerForgeOAuthApp);

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <RegisterOAuthAppForm teamID="t1" connections={[]} onRegistered={vi.fn()} onError={vi.fn()} />
    </QueryClientProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "+ Register an OAuth app" }));
}

beforeEach(() => {
  vi.clearAllMocks();
  registerMock.mockResolvedValue({ id: "app-1" } as never);
});
afterEach(cleanup);

// The hook base an instance accepts is said once, on the app, and inherited
// by every connection made through it: the manual registration carries it.
describe("RegisterOAuthAppForm — webhook base URL", () => {
  it("sends the pasted credentials with the webhook base in manual mode", async () => {
    mount();
    fireEvent.click(screen.getByRole("radio", { name: /Paste credentials/ }));
    fireEvent.change(screen.getByLabelText("Forge base URL"), {
      target: { value: "https://pic.example" },
    });
    fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "cid" } });
    fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: "secret" } });
    fireEvent.change(screen.getByLabelText("Webhook base URL"), {
      target: { value: " https://iterion.fabrique.example " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Register" }));
    await waitFor(() => expect(registerMock).toHaveBeenCalledTimes(1));
    expect(registerMock).toHaveBeenCalledWith("t1", {
      provider: "gitlab",
      forge_base_url: "https://pic.example",
      mode: "manual",
      client_id: "cid",
      client_secret: "secret",
      webhook_base_url: "https://iterion.fabrique.example",
    });
  });

  it("sends no webhook base when the field stays empty", async () => {
    mount();
    fireEvent.click(screen.getByRole("radio", { name: /Paste credentials/ }));
    fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "cid" } });
    fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: "secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Register" }));
    await waitFor(() => expect(registerMock).toHaveBeenCalledTimes(1));
    expect(registerMock.mock.calls[0]?.[1]?.webhook_base_url).toBeUndefined();
  });
});
