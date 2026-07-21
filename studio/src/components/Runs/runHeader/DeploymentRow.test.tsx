// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { DeploymentReport, DeploymentTrace } from "@/api/runs";
import DeploymentRow from "./DeploymentRow";
import { traceabilityState } from "./deploymentTraceability";

afterEach(cleanup);

const traceable: DeploymentTrace = {
  verifiable: true,
  pushed: true,
  image_from_repo: true,
  built_from_head: true,
  log: "image=ghcr.io/acme/app:abc1234 | repo=acme/app",
};

function report(over: Partial<DeploymentReport> = {}): DeploymentReport {
  return {
    node_id: "deploy",
    deployed: true,
    healthy: true,
    url: "https://app.example.test",
    image_ref: "ghcr.io/acme/app:abc1234",
    commit: "abc1234def5678",
    ...over,
  };
}

describe("traceabilityState", () => {
  it("separates the four causes", () => {
    expect(traceabilityState(traceable)).toBe("traceable");
    expect(traceabilityState({ ...traceable, pushed: false })).toBe(
      "untraceable",
    );
    // verifiable:false wins over the (meaningless) booleans below it.
    expect(
      traceabilityState({ ...traceable, verifiable: false, pushed: false }),
    ).toBe("unverified");
    expect(traceabilityState(undefined)).toBe("unchecked");
  });
});

describe("DeploymentRow", () => {
  it("makes the live URL the primary, clickable element", () => {
    render(<DeploymentRow deployment={report()} />);
    const link = screen.getByRole("link", { name: /app\.example\.test/ });
    expect(link.getAttribute("href")).toBe("https://app.example.test");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toContain("noopener");
  });

  it("surfaces the image reference, the commit and health", () => {
    render(<DeploymentRow deployment={report()} />);
    expect(screen.getByText("ghcr.io/acme/app:abc1234")).toBeTruthy();
    // Short SHA, like FinalizationRow.
    expect(screen.getByText("abc1234")).toBeTruthy();
    expect(screen.getByText("healthy")).toBeTruthy();
  });

  // The three states the deployment contract names must be tellable
  // apart by BOTH copy and tone — an operator scanning the row has to
  // see at a glance which one they're in.
  describe("traceability states render distinguishably", () => {
    const chipOf = (container: HTMLElement, label: string | RegExp) => {
      const node = screen.getByText(label);
      expect(container.contains(node)).toBe(true);
      return node;
    };

    it("traceable: green, no caveat", () => {
      const { container } = render(
        <DeploymentRow deployment={report({ trace: traceable })} />,
      );
      const chip = chipOf(container, "traceable");
      expect(chip.className).toContain("bg-success-soft");
      expect(screen.queryByText(/not traceable/)).toBeNull();
      expect(screen.queryByText(/unverified/)).toBeNull();
    });

    it("not traceable: red, and names WHICH facts are false", () => {
      const { container } = render(
        <DeploymentRow
          deployment={report({
            // The ConfigMap-on-a-stock-base-image failure: live URL,
            // honest liveness fields, nothing delivered.
            image_ref: "node:22-slim",
            trace: {
              verifiable: true,
              pushed: false,
              image_from_repo: false,
              built_from_head: true,
            },
          })}
        />,
      );
      const chip = chipOf(container, "not traceable");
      expect(chip.className).toContain("bg-danger-soft");
      expect(screen.getByText(/not pushed/)).toBeTruthy();
      expect(screen.getByText(/image not from this repo/)).toBeTruthy();
      // built_from_head was true, so it is not listed as a reason.
      expect(screen.queryByText(/doesn't name the commit/)).toBeNull();
    });

    it("unverified: neither green nor red — not a failure", () => {
      const { container } = render(
        <DeploymentRow
          deployment={report({
            trace: {
              verifiable: false,
              pushed: false,
              image_from_repo: false,
              built_from_head: false,
              log: "CANNOT VERIFY: git is unavailable in this workspace",
            },
          })}
        />,
      );
      const chip = chipOf(container, "traceability unverified");
      expect(chip.className).toContain("bg-info-soft");
      expect(chip.className).not.toContain("bg-danger-soft");
      expect(chip.className).not.toContain("bg-success-soft");
      // The unverifiable booleans carry no information and must not be
      // reported as failed facts.
      expect(screen.queryByText(/not pushed/)).toBeNull();
    });

    it("no gate at all: says so rather than implying the URL is trusted", () => {
      render(<DeploymentRow deployment={report({ trace: undefined })} />);
      expect(screen.getByText("traceability not checked")).toBeTruthy();
    });
  });

  it("renders a failed deploy honestly instead of hiding it", () => {
    render(
      <DeploymentRow
        deployment={report({
          deployed: false,
          healthy: false,
          url: undefined,
          image_ref: undefined,
          commit: undefined,
          notes: "no deploy-target skill attached",
        })}
      />,
    );
    expect(screen.getByText("not deployed")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("no deploy-target skill attached")).toBeTruthy();
    expect(screen.queryByText("healthy")).toBeNull();
    // Nothing was delivered, so there is no URL for a traceability
    // verdict to qualify — the chip would read as a second failure.
    expect(screen.queryByText(/traceability/)).toBeNull();
  });

  it("flags a deploy that claims success but names no URL", () => {
    render(<DeploymentRow deployment={report({ url: undefined })} />);
    expect(screen.getByText("no URL reported")).toBeTruthy();
  });

  it("flags a deploy that applied but did not come up healthy", () => {
    const { container } = render(
      <DeploymentRow deployment={report({ healthy: false })} />,
    );
    const chip = screen.getByText("not healthy");
    expect(container.contains(chip)).toBe(true);
    expect(chip.className).toContain("bg-warning-soft");
  });

  it("omits the commit when the report states none, never guessing one", () => {
    render(<DeploymentRow deployment={report({ commit: undefined })} />);
    expect(screen.queryByText("commit")).toBeNull();
  });
});
