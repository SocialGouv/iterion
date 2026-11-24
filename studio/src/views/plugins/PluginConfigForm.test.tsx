// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { PluginView } from "@/api/plugins";

vi.mock("@/api/plugins", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/plugins")>();
  return { ...actual, setPluginConfig: vi.fn() };
});

import PluginConfigForm from "./PluginConfigForm";

const plugin: PluginView = {
  name: "notifier",
  enabled: true,
  builtin: false,
  kinds: [],
  config_schema: [
    { key: "endpoint", type: "string" },
    { key: "api_token", type: "secret" },
  ],
  config_values: { endpoint: "https://hooks.example.test" },
  config_secret_set: ["api_token"],
};

afterEach(cleanup);

describe("PluginConfigForm — a secret field", () => {
  // A secret field starts blank and a blank submission keeps the stored value.
  // A saved login filled in by the browser makes the form dirty and would be
  // saved over the secret; autocomplete="off" does not stop it on a password
  // input.
  it("keeps the browser's saved login out of the secret input", () => {
    render(<PluginConfigForm plugin={plugin} onSaved={() => {}} />);
    const secret = screen.getByPlaceholderText(/leave blank to keep/) as HTMLInputElement;
    expect(secret.type).toBe("password");
    expect(secret.getAttribute("autocomplete")).toBe("new-password");
  });

  it("leaves a plain field to the browser's own completion", () => {
    render(<PluginConfigForm plugin={plugin} onSaved={() => {}} />);
    const endpoint = screen.getByDisplayValue("https://hooks.example.test") as HTMLInputElement;
    expect(endpoint.type).toBe("text");
    expect(endpoint.hasAttribute("autocomplete")).toBe(false);
  });
});
