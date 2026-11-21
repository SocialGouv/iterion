// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { SecretStatus } from "@/lib/desktopBridge";

const statuses: SecretStatus[] = [
  { key: "ANTHROPIC_API_KEY", stored: true, shadowed: false },
  { key: "OPENAI_API_KEY", stored: false, shadowed: false },
];
vi.mock("@/lib/desktopBridge", () => ({
  desktop: {
    getSecretStatuses: vi.fn(async () => statuses),
    setSecret: vi.fn(),
    deleteSecret: vi.fn(),
  },
}));

import ApiKeysEditor from "./ApiKeysEditor";

afterEach(cleanup);

describe("ApiKeysEditor", () => {
  // Each row is a lone password input. With no hint a browser fills the saved
  // login's password into it, and "off" is ignored on this input type.
  it("keeps the browser's saved login out of every key input", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={qc}>
        <ApiKeysEditor />
      </QueryClientProvider>,
    );
    await screen.findByText("ANTHROPIC_API_KEY");
    const inputs = [
      screen.getByPlaceholderText(/\(stored\)/),
      screen.getByPlaceholderText("Enter value"),
    ] as HTMLInputElement[];
    for (const input of inputs) {
      expect(input.type).toBe("password");
      expect(input.getAttribute("autocomplete")).toBe("new-password");
    }
  });
});
