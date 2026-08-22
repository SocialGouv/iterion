import { describe, expect, it } from "vitest";

import type { FallbackDecl } from "@/api/types";
import {
  compactEnvRef,
  displayFallbackChain,
  displayModel,
  envDefault,
  modelTooltip,
  shortenModel,
} from "./modelLabel";

describe("shortenModel", () => {
  it("keeps the last path segment so sol/terra/luna stay visible", () => {
    expect(shortenModel("openai-codex/gpt-5.6-sol")).toBe("gpt-5.6-sol");
    expect(shortenModel("openai-codex/gpt-5.6-terra")).toBe("gpt-5.6-terra");
    expect(shortenModel("anthropic/claude-opus-5")).toBe("claude-opus-5");
  });

  it("leaves a bare id alone", () => {
    expect(shortenModel("claude-opus-5")).toBe("claude-opus-5");
    expect(shortenModel("gpt-5.6-luna")).toBe("gpt-5.6-luna");
  });
});

describe("envDefault", () => {
  it("extracts the authored default of a ${VAR:-default} form", () => {
    expect(envDefault("${CODEX_MODEL:-openai-codex/gpt-5.6-sol}")).toBe(
      "openai-codex/gpt-5.6-sol",
    );
  });

  it("returns undefined for a bare ${VAR} (no default to show)", () => {
    expect(envDefault("${CODEX_MODEL}")).toBeUndefined();
  });

  it("returns undefined for a plain spec", () => {
    expect(envDefault("openai-codex/gpt-5.6-sol")).toBeUndefined();
  });
});

describe("displayModel", () => {
  it("prefers the server-resolved spec over the authored default", () => {
    expect(
      displayModel(
        "${CODEX_MODEL:-openai-codex/gpt-5.6-sol}",
        "openai-codex/gpt-5.6-terra",
      ),
    ).toBe("gpt-5.6-terra");
  });

  it("falls back to the authored default while the server is loading", () => {
    expect(displayModel("${CODEX_MODEL:-openai-codex/gpt-5.6-sol}")).toBe(
      "gpt-5.6-sol",
    );
  });

  it("does not collapse ${VAR} to the word env", () => {
    expect(displayModel("${CODEX_MODEL}")).toBe("$CODEX_MODEL");
    expect(compactEnvRef("${CODEX_MODEL}")).toBe("$CODEX_MODEL");
    expect(displayModel("${CODEX_MODEL}")).not.toBe("env");
    expect(displayModel("${CODEX_MODEL:-openai-codex/gpt-5.6-sol}")).not.toBe("env");
  });

  it("shortens a literal spec", () => {
    expect(displayModel("openai-codex/gpt-5.6-luna")).toBe("gpt-5.6-luna");
  });
});

describe("modelTooltip", () => {
  it("shows both the live expansion and the declared literal", () => {
    expect(
      modelTooltip(
        "${CODEX_MODEL:-openai-codex/gpt-5.6-sol}",
        "openai-codex/gpt-5.6-terra",
      ),
    ).toBe(
      "model: openai-codex/gpt-5.6-terra\ndeclared: ${CODEX_MODEL:-openai-codex/gpt-5.6-sol}",
    );
  });
});

describe("displayFallbackChain", () => {
  const fallbacks: FallbackDecl[] = [
    { name: "terra", model: "openai-codex/gpt-5.6-terra" },
    { name: "api", backend: "claw", model: "openai/gpt-5.5", metered: true },
  ];

  it("joins routes in declaration order", () => {
    expect(displayFallbackChain(fallbacks)).toBe(
      "gpt-5.6-terra → claw/gpt-5.5",
    );
  });

  it("uses resolved models when provided", () => {
    expect(
      displayFallbackChain(fallbacks, [
        "openai-codex/gpt-5.6-luna",
        "openai/gpt-5.5",
      ]),
    ).toBe("gpt-5.6-luna → claw/gpt-5.5");
  });

  it("returns undefined for an empty chain", () => {
    expect(displayFallbackChain([])).toBeUndefined();
    expect(displayFallbackChain(undefined)).toBeUndefined();
  });
});
