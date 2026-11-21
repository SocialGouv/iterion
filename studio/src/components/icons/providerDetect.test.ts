import { describe, expect, it } from "vitest";

import { detectProvider } from "./providerDetect";

describe("detectProvider", () => {
  it("reads the vendor from the model prefix", () => {
    expect(detectProvider("anthropic/claude-sonnet-4")?.iconId).toBe("Claude");
    expect(detectProvider("deepseek/deepseek-chat")?.iconId).toBe("DeepSeek");
    expect(detectProvider("openai/gpt-4o")?.iconId).toBe("OpenAI");
    // The prefix decides before the keywords: "gpt" alone would say OpenAI.
    expect(detectProvider("openai-codex/gpt-5.6-sol")?.iconId).toBe("Codex");
  });

  it("lets the delegate win over the model", () => {
    expect(detectProvider("openai/gpt-4o", "claude_code")?.iconId).toBe("Claude");
    expect(detectProvider("ollama/${OPENAI_COMPATIBLE_MODEL}", "claude_code")?.iconId).toBe("Claude");
  });

  it.each([
    ["ollama/${OPENAI_COMPATIBLE_MODEL:-qwen3}", "Ollama"],
    ["google/${OPENAI_COMPATIBLE_MODEL}", "Gemini"],
    ["OpenAI-Codex/${OPENAI_COMPATIBLE_MODEL:-gpt-5.6-sol}", "Codex"],
  ])("keeps the explicit vendor in %s", (model, icon) => {
    expect(detectProvider(model)?.iconId).toBe(icon);
  });

  it.each([
    "openai_compatible/ollama/qwen3",
    "OPENAI_COMPATIBLE/OLLAMA/llama3:8b",
    "${X:-ollama/llama3:8b}",
  ])("does not mistake ollama for its llama substring in %s", (model) => {
    expect(detectProvider(model)?.iconId).toBe("Ollama");
  });

  it("reads past the gateway protocol of a route", () => {
    // "openai_compatible" names the wire protocol of a gateway route, not the
    // vendor of the model it serves.
    expect(detectProvider("openai_compatible/scaleway/deepseek-v4.1-flash")?.iconId).toBe("DeepSeek");
    expect(detectProvider("openai_compatible/mistral-large-latest")?.iconId).toBe("Mistral");
    expect(detectProvider("OpenAI_Compatible/scaleway/DeepSeek-V4.1-Flash")?.iconId).toBe("DeepSeek");
  });

  it("reads past the protocol name inside an unresolved env reference", () => {
    // The run graph and the canvas pass the raw literal when nothing resolved it.
    expect(detectProvider("${ITERION_MODEL:-openai_compatible/scaleway/deepseek-v4.1-flash}")?.iconId).toBe("DeepSeek");
    expect(detectProvider("${OPENAI_COMPATIBLE_MODEL:-gpt-4o}")?.iconId).toBe("OpenAI");
    // A variable named after the protocol names no vendor.
    expect(detectProvider("${OPENAI_COMPATIBLE_MODEL}")).toBeNull();
  });

  it("does not read a gateway's own namespace as the vendor", () => {
    // The first segment of a gateway id names the upstream host, and the model
    // behind it keeps its vendor.
    expect(detectProvider("openai_compatible/azure/claude-opus-5-5")?.iconId).toBe("Claude");
    expect(detectProvider("openai_compatible/bedrock/claude-sonnet-4-6")?.iconId).toBe("Claude");
    expect(detectProvider("openai_compatible/azure/gpt-4o")?.iconId).toBe("OpenAI");
    expect(detectProvider("openai_compatible/together/llama-3.3-70b")?.iconId).toBe("Meta");
    // Known gateway namespaces still decide when no model keyword does.
    expect(detectProvider("openai_compatible/bedrock/unknown")?.iconId).toBe("Bedrock");
    expect(detectProvider("openai_compatible/groq/unknown")?.iconId).toBe("Groq");
  });

  it("ignores the protocol name every time a spec carries it", () => {
    expect(
      detectProvider("${OPENAI_COMPATIBLE_MODEL:-openai_compatible/scaleway/deepseek-v4.1-flash}")?.iconId,
    ).toBe("DeepSeek");
  });

  it("still shows OpenAI for an OpenAI model served through a gateway", () => {
    expect(detectProvider("openai_compatible/gpt-4o")?.iconId).toBe("OpenAI");
    expect(detectProvider("openai_compatible/openai/gpt-4o")?.iconId).toBe("OpenAI");
  });

  it("names no vendor for a gateway model it does not recognise", () => {
    expect(detectProvider("openai_compatible/scaleway/glm-5.3")).toBeNull();
  });
});
