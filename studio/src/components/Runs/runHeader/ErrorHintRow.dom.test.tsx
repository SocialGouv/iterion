// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { RunHeader } from "@/api/runs";
import ErrorHintRow from "./ErrorHintRow";

afterEach(cleanup);

function failedRun(fields: Partial<RunHeader> = {}): RunHeader {
  return { status: "failed", ...fields } as RunHeader;
}

describe("ErrorHintRow", () => {
  it("uses the persisted code even when the message wraps or contradicts it", () => {
    render(<ErrorHintRow run={failedRun({ failure_code: "SANDBOX_DRIVER_UNAVAILABLE", error: "sandbox start: [TIMEOUT] no container runtime" })} onResume={vi.fn()} />);
    expect(screen.getByText(/install Docker or Podman/)).toBeTruthy();
    expect(screen.getByText(/Kubernetes configuration and access in cloud/)).toBeTruthy();
    expect(screen.queryByText(/Increase `max_duration`/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Resume/ })).toBeNull();
  });

  it("shows a typed failure even when the message is absent", () => {
    render(<ErrorHintRow run={failedRun({ failure_code: "SANDBOX_DRIVER_UNAVAILABLE" })} onResume={vi.fn()} />);
    expect(screen.getByText(/install Docker or Podman/)).toBeTruthy();
    expect(screen.getByText(/Kubernetes configuration and access in cloud/)).toBeTruthy();
  });

  it("retains hints for legacy prefixed errors and their resume action", () => {
    render(<ErrorHintRow run={failedRun({ status: "failed_resumable", error: "[BUDGET_EXCEEDED] exhausted" })} onResume={vi.fn()} />);
    expect(screen.getByText(/Budget overrides/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Resume/ })).toBeTruthy();
  });

  it("does not resurrect a stale failure on a healthy run", () => {
    render(<ErrorHintRow run={failedRun({ status: "finished", failure_code: "SANDBOX_DRIVER_UNAVAILABLE", error: "old" })} onResume={vi.fn()} />);
    expect(screen.queryByText(/install Docker or Podman/)).toBeNull();
  });
});
