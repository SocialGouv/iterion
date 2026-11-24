import { describe, expect, it } from "vitest";
import { inboundWebhookURL, providerSetupSnippet } from "@/api/webhooks";

describe("inboundWebhookURL", () => {
  it("builds the canonical /api/webhooks/{provider}/{id} path", () => {
    const url = inboundWebhookURL("gitlab", "wh_123", "https://iterion.example.com");
    expect(url).toBe("https://iterion.example.com/api/webhooks/gitlab/wh_123");
  });

  it("strips a trailing slash from the origin", () => {
    const url = inboundWebhookURL("github", "wh_42", "https://iterion.example.com/");
    expect(url).toBe("https://iterion.example.com/api/webhooks/github/wh_42");
  });

  it("encodes the webhook id", () => {
    const url = inboundWebhookURL("generic", "abc/def", "https://iterion.example.com");
    expect(url).toBe("https://iterion.example.com/api/webhooks/generic/abc%2Fdef");
  });

  it("routes a connector ingress to /api/webhooks/c/{id}", () => {
    const url = inboundWebhookURL("connector", "ing-1", "https://iterion.example.com");
    expect(url).toBe("https://iterion.example.com/api/webhooks/c/ing-1");
  });
});

describe("providerSetupSnippet", () => {
  it("returns GitLab-specific steps for gitlab provider", () => {
    const s = providerSetupSnippet("gitlab", "https://x/api/webhooks/gitlab/wh", "iwh_abc");
    expect(s.title.toLowerCase()).toContain("gitlab");
    expect(s.steps.join(" ").toLowerCase()).toContain("secret token");
    expect(s.example).toContain("iwh_abc");
  });

  it("returns GitHub-specific steps for github provider", () => {
    const s = providerSetupSnippet("github", "https://x/api/webhooks/github/wh", "iwh_abc");
    expect(s.title.toLowerCase()).toContain("github");
    expect(s.steps.join(" ").toLowerCase()).toContain("pull requests");
  });

  it("returns a curl example for generic provider", () => {
    const s = providerSetupSnippet("generic", "https://x/api/webhooks/generic/wh", "iwh_42");
    expect(s.example).toContain("curl");
    expect(s.example).toContain("X-Iterion-Webhook-Token: iwh_42");
  });
});

describe("providerSetupSnippet for a connector ingress", () => {
  it("shows a minted secret, and no secret line for the vendor's own", () => {
    const minted = providerSetupSnippet("connector", "https://x/api/webhooks/c/ing", "iwh_abc");
    expect(minted.example).toContain("iwh_abc");
    const vendor = providerSetupSnippet("connector", "https://x/api/webhooks/c/ing", "");
    expect(vendor.example).toBe("# URL\nhttps://x/api/webhooks/c/ing");
    expect(vendor.steps.join(" ")).toContain("vendor's own signing secret");
  });
});
