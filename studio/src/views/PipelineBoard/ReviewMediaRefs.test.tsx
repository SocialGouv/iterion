// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectNoViolations } from "@/__tests__/a11y/axeHelpers";
import type { PreviewState } from "@/components/Runs/usePreview";

vi.mock("./ArtifactFilePreviewDialog", () => ({
  ArtifactFilePreviewDialog: (props: {
    preview: PreviewState;
    runId: string;
    kind: string;
    imageAlt?: string;
  }) => (
    <div
      data-testid="artifact-preview-dialog"
      data-run-id={props.runId}
      data-kind={props.kind}
      data-path={props.preview.path}
      data-src={props.preview.blobURL}
      data-loading={String(props.preview.loading)}
      data-content-type={props.preview.contentType}
      data-image-alt={props.imageAlt}
    />
  ),
}));

import { ReviewMediaRefs } from "./ReviewMediaRefs";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ReviewMediaRefs", () => {
  it("has no automated WCAG A/AA violations as an attached-media carousel", async () => {
    const { container } = render(
      <ReviewMediaRefs
        fallbackRunId="run-review"
        media={[
          {
            path: "renders/concept.png",
            kind: "image",
            caption: "Concept",
          },
          {
            path: "notes/plan.md",
            kind: "doc",
            caption: "Plan notes",
          },
        ]}
      />,
    );

    await expectNoViolations(container, "Attached review media carousel");
  });

  it("keeps media in order and opens the active item with explicit/fallback run ids", () => {
    render(
      <ReviewMediaRefs
        fallbackRunId="run-review"
        media={[
          {
            run_id: "run-child",
            path: "renders/final cut.mp4",
            kind: "video",
            mime: "video/mp4",
            size: 4096,
            caption: "Validate motion and timing",
          },
          {
            path: "audio/theme.wav",
            kind: "audio",
            caption: "Check the mix",
          },
        ]}
      />,
    );

    expect(screen.getByText("Validate motion and timing")).toBeTruthy();
    expect(screen.queryByText("Check the mix")).toBeNull();
    expect(screen.getByText("1 / 2")).toBeTruthy();
    expect(
      (screen.getByRole("button", {
        name: "Previous review media",
      }) as HTMLButtonElement).disabled,
    ).toBe(true);

    const firstDownload = screen.getByRole("link", {
      name: "Download renders/final cut.mp4",
    });
    expect(firstDownload.getAttribute("href")).toContain(
      "/runs/run-child/artifact-files/renders/final%20cut.mp4?download=1",
    );

    fireEvent.click(
      screen.getByRole("button", {
        name: "Open Validate motion and timing",
      }),
    );
    let dialog = screen.getByTestId("artifact-preview-dialog");
    expect(dialog.getAttribute("data-run-id")).toBe("run-child");
    expect(dialog.getAttribute("data-path")).toBe("renders/final cut.mp4");
    expect(dialog.getAttribute("data-src")).toContain(
      "/runs/run-child/artifact-files/renders/final%20cut.mp4",
    );
    expect(dialog.getAttribute("data-loading")).toBe("false");
    expect(dialog.getAttribute("data-content-type")).toBe("video/mp4");

    fireEvent.click(
      screen.getByRole("button", { name: "Next review media" }),
    );
    expect(screen.getByText("2 / 2")).toBeTruthy();
    expect(screen.queryByText("Validate motion and timing")).toBeNull();
    expect(screen.getByText("Check the mix")).toBeTruthy();
    expect(
      (screen.getByRole("button", {
        name: "Next review media",
      }) as HTMLButtonElement).disabled,
    ).toBe(true);

    fireEvent.click(
      screen.getByRole("button", { name: "Open Check the mix" }),
    );
    dialog = screen.getByTestId("artifact-preview-dialog");
    expect(dialog.getAttribute("data-run-id")).toBe("run-review");
    expect(dialog.getAttribute("data-path")).toBe("audio/theme.wav");
    expect(dialog.getAttribute("data-src")).toContain(
      "/runs/run-review/artifact-files/audio/theme.wav",
    );
  });

  it("passes the selected media kind and run to the shared preview", () => {
    render(
      <ReviewMediaRefs
        fallbackRunId="run-review"
        media={[{ path: "clip.webm", kind: "video" }]}
      />,
    );

    expect(screen.queryByTestId("artifact-preview-dialog")).toBeNull();
    fireEvent.click(
      screen.getByRole("button", { name: "Open Review video" }),
    );
    const dialog = screen.getByTestId("artifact-preview-dialog");
    expect(dialog.getAttribute("data-run-id")).toBe("run-review");
    expect(dialog.getAttribute("data-kind")).toBe("video");
    expect(dialog.getAttribute("data-content-type")).toBe("application/octet-stream");
    expect(screen.getByText("1 / 1")).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "Previous review media" }),
    ).toBeNull();
    expect(
      screen.queryByRole("button", { name: "Next review media" }),
    ).toBeNull();
  });

  it("passes the selected image caption to the shared preview", () => {
    render(
      <ReviewMediaRefs
        fallbackRunId="run-review"
        media={[
          {
            path: "cover.png",
            kind: "image",
            caption: "Final cover artwork",
          },
        ]}
      />,
    );

    expect(screen.getByRole("img", { name: "Final cover artwork" })).toBeTruthy();
    fireEvent.click(
      screen.getByRole("button", { name: "Open Final cover artwork" }),
    );
    expect(
      screen.getByTestId("artifact-preview-dialog").getAttribute("data-image-alt"),
    ).toBe("Final cover artwork");
  });

  it("supports bounded buttons and Arrow, Home and End keyboard navigation", () => {
    render(
      <ReviewMediaRefs
        fallbackRunId="run-review"
        media={[
          {
            path: "renders/concept.png",
            kind: "image",
            caption: "Concept",
          },
          {
            path: "notes/plan.md",
            kind: "doc",
            caption: "Plan notes",
          },
          {
            path: "renders/walkthrough.webm",
            kind: "video",
            caption: "Walkthrough",
          },
        ]}
      />,
    );

    const carousel = screen.getByRole("group", {
      name: "Attached review media carousel",
    });
    expect(carousel.getAttribute("aria-roledescription")).toBe("carousel");
    expect(carousel.getAttribute("aria-keyshortcuts")).toBe(
      "ArrowLeft ArrowRight Home End",
    );
    expect(screen.getByText("1 / 3")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Concept" })).toBeTruthy();
    expect(screen.queryByText("Plan notes")).toBeNull();

    fireEvent.click(
      screen.getByRole("button", { name: "Next review media" }),
    );
    expect(screen.getByText("2 / 3")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Open Plan notes" }),
    ).toBeTruthy();

    fireEvent.keyDown(carousel, { key: "End" });
    expect(screen.getByText("3 / 3")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Open Walkthrough" }),
    ).toBeTruthy();
    expect(
      (screen.getByRole("button", {
        name: "Next review media",
      }) as HTMLButtonElement).disabled,
    ).toBe(true);

    fireEvent.keyDown(carousel, { key: "ArrowRight" });
    expect(screen.getByText("3 / 3")).toBeTruthy();
    fireEvent.keyDown(carousel, { key: "Home" });
    expect(screen.getByText("1 / 3")).toBeTruthy();
    fireEvent.keyDown(carousel, { key: "ArrowRight" });
    expect(screen.getByText("2 / 3")).toBeTruthy();
    fireEvent.keyDown(carousel, { key: "ArrowLeft" });
    expect(screen.getByText("1 / 3")).toBeTruthy();
  });

  it("resets the active slide and preview when the ordered media payload changes", () => {
    const view = render(
      <ReviewMediaRefs
        fallbackRunId="run-review"
        media={[
          { path: "first.png", kind: "image", caption: "First image" },
          { path: "second.mp4", kind: "video", caption: "Second video" },
        ]}
      />,
    );

    fireEvent.click(
      screen.getByRole("button", { name: "Next review media" }),
    );
    fireEvent.click(
      screen.getByRole("button", { name: "Open Second video" }),
    );
    expect(screen.getByText("2 / 2")).toBeTruthy();
    expect(screen.getByTestId("artifact-preview-dialog")).toBeTruthy();

    view.rerender(
      <ReviewMediaRefs
        fallbackRunId="run-review"
        media={[
          { path: "replacement.png", kind: "image", caption: "Replacement" },
          { path: "extra.wav", kind: "audio", caption: "Extra audio" },
        ]}
      />,
    );

    expect(screen.getByText("1 / 2")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Replacement" })).toBeTruthy();
    expect(screen.queryByText("Second video")).toBeNull();
    expect(screen.queryByTestId("artifact-preview-dialog")).toBeNull();
  });

  it("renders nothing for an empty attachment list", () => {
    const { container } = render(
      <ReviewMediaRefs fallbackRunId="run-review" media={[]} />,
    );
    expect(container.innerHTML).toBe("");
  });
});
