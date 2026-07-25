// @vitest-environment jsdom

import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectNoViolations } from "@/__tests__/a11y/axeHelpers";

const apiMocks = vi.hoisted(() => ({
  getRunFileContent: vi.fn(),
}));

vi.mock("@/api/runs", () => ({
  getRunFileContent: apiMocks.getRunFileContent,
  runFilePreviewURL: (runId: string, path: string) =>
    `/api/runs/${encodeURIComponent(runId)}/files/preview/${path
      .split("/")
      .map(encodeURIComponent)
      .join("/")}`,
}));

vi.mock("./ImagePreview", () => ({
  ImagePreviewDialog: (props: {
    open: boolean;
    src: string;
    alt: string;
    title?: React.ReactNode;
    description?: React.ReactNode;
    onOpenChange: (open: boolean) => void;
  }) =>
    props.open ? (
      <div
        role="dialog"
        aria-label="Image preview"
        data-src={props.src}
        data-alt={props.alt}
      >
        <span>{props.title}</span>
        <span>{props.description}</span>
        <button type="button" onClick={() => props.onOpenChange(false)}>
          Close image preview
        </button>
      </div>
    ) : null,
}));

import {
  ReviewWorkspaceFiles,
  reviewWorkspaceFilesFromQuestions,
} from "./ReviewWorkspaceFiles";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("reviewWorkspaceFilesFromQuestions", () => {
  it("extracts safe previewable paths from direct, nested and encoded values", () => {
    const files = reviewWorkspaceFilesFromQuestions({
      review_image_path: "renders/review-board.PNG",
      plan_path: "plans/vertical-plan.json",
      supporting_files: [
        "notes/read me.md",
        {
          path: "data/player-feedback.csv",
          caption: "Feedback from playtesters",
        },
      ],
      encoded_files: JSON.stringify([
        "config/review.yaml",
        "config/review.YML",
      ]),
      duplicate_path: "plans/vertical-plan.json",
      unsupported_path: "exports/archive.zip",
      remote_image_path: "https://example.test/render.png",
      absolute_path: "/etc/passwd.txt",
      traversal_path: "renders/../secret.png",
      notes: "See the final image in renders/hidden.png",
    });

    expect(files).toEqual([
      {
        path: "renders/review-board.PNG",
        name: "review-board.PNG",
        label: "Review image",
        kind: "image",
        extension: "png",
      },
      {
        path: "plans/vertical-plan.json",
        name: "vertical-plan.json",
        label: "Plan",
        kind: "text",
        extension: "json",
      },
      {
        path: "notes/read me.md",
        name: "read me.md",
        label: "Supporting",
        kind: "text",
        extension: "md",
      },
      {
        path: "data/player-feedback.csv",
        name: "player-feedback.csv",
        label: "Feedback from playtesters",
        kind: "text",
        extension: "csv",
      },
      {
        path: "config/review.yaml",
        name: "review.yaml",
        label: "Encoded",
        kind: "text",
        extension: "yaml",
      },
      {
        path: "config/review.YML",
        name: "review.YML",
        label: "Encoded",
        kind: "text",
        extension: "yml",
      },
    ]);
  });

  it("returns an empty list when questions contain no previewable files", () => {
    expect(
      reviewWorkspaceFilesFromQuestions({
        approved: "Approve this plan?",
        count: 3,
        metadata: { archive: "output.tar.gz" },
      }),
    ).toEqual([]);
  });

  it("uses the authored instruction order before question-only extras", () => {
    const review = "renders/review.png";
    const concept = "renders/concept.png";
    const plan = "plans/vertical-plan.json";

    const files = reviewWorkspaceFilesFromQuestions(
      {
        concept_image_path: concept,
        plan_path: plan,
        review_image_path: review,
      },
      [
        `- \`${review}\` : Storyboard to review;`,
        `- \`${concept}\` : Full concept;`,
      ].join("\n"),
    );

    expect(files.map((file) => file.path)).toEqual([review, concept, plan]);
    expect(files.map((file) => file.label)).toEqual([
      "Storyboard to review",
      "Full concept",
      "Plan",
    ]);
  });
});

describe("ReviewWorkspaceFiles", () => {
  it("has no automated WCAG A/AA violations as a mixed-file carousel", async () => {
    const { container } = render(
      <ReviewWorkspaceFiles
        runId="run-review"
        questions={{
          review_image_path: "renders/review.png",
          plan_path: "plans/vertical-plan.json",
        }}
      />,
    );

    await expectNoViolations(container, "Review workspace files carousel");
  });

  it("returns null when there is nothing to preview", () => {
    const { container } = render(
      <ReviewWorkspaceFiles
        runId="run-1"
        questions={{ approved: "Approve?" }}
      />,
    );

    expect(container.innerHTML).toBe("");
  });

  it("shows a large image slide with human labels and opens the Iterion image viewer", () => {
    const path = "iterion/state/concepts/final-review.png";
    render(
      <ReviewWorkspaceFiles
        runId="run-1"
        questions={{ review_image_path: path }}
        instructions={`- \`${path}\` : storyboard to approve;`}
      />,
    );

    expect(screen.getByText("Storyboard to approve")).toBeTruthy();
    expect(screen.getByText("final-review.png")).toBeTruthy();
    expect(screen.queryByText(path)).toBeNull();

    const image = screen.getByRole("img", { name: "Storyboard to approve" });
    expect(image.getAttribute("src")).toBe(
      "/api/runs/run-1/files/preview/iterion/state/concepts/final-review.png",
    );
    fireEvent.load(image);
    expect(
      screen.queryByRole("status", { name: "Loading Storyboard to approve" }),
    ).toBeNull();

    fireEvent.click(
      screen.getByRole("button", {
        name: "Open Storyboard to approve (final-review.png)",
      }),
    );
    const viewer = screen.getByRole("dialog", { name: "Image preview" });
    expect(viewer.getAttribute("data-src")).toContain(
      "/runs/run-1/files/preview/iterion/state/concepts/final-review.png",
    );
    expect(viewer.getAttribute("data-alt")).toBe("Storyboard to approve");
    expect(screen.getByText("1 / 1")).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "Previous review file" }),
    ).toBeNull();
    expect(
      screen.queryByRole("button", { name: "Next review file" }),
    ).toBeNull();
  });

  it("keeps mixed files in detection order with controls and keyboard navigation", () => {
    render(
      <ReviewWorkspaceFiles
        runId="run-review"
        questions={{
          concept_image_path: "renders/concept.png",
          plan_path: "plans/vertical-plan.json",
          review_image_path: "renders/review.png",
        }}
      />,
    );

    const carousel = screen.getByRole("group", {
      name: "Review files carousel",
    });
    expect(carousel.getAttribute("aria-roledescription")).toBe("carousel");
    expect(screen.getByText("1 / 3")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Concept image" })).toBeTruthy();
    expect(
      screen.queryByRole("button", {
        name: "Open Plan (vertical-plan.json)",
      }),
    ).toBeNull();
    expect(
      (screen.getByRole("button", {
        name: "Previous review file",
      }) as HTMLButtonElement).disabled,
    ).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Next review file" }));
    expect(screen.getByText("2 / 3")).toBeTruthy();
    expect(
      screen.getByRole("button", {
        name: "Open Plan (vertical-plan.json)",
      }),
    ).toBeTruthy();
    expect(screen.queryByRole("img", { name: "Concept image" })).toBeNull();

    fireEvent.keyDown(carousel, { key: "End" });
    expect(screen.getByText("3 / 3")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Review image" })).toBeTruthy();
    expect(
      (screen.getByRole("button", {
        name: "Next review file",
      }) as HTMLButtonElement).disabled,
    ).toBe(true);

    fireEvent.keyDown(carousel, { key: "Home" });
    expect(screen.getByText("1 / 3")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Concept image" })).toBeTruthy();

    fireEvent.keyDown(carousel, { key: "ArrowRight" });
    expect(screen.getByText("2 / 3")).toBeTruthy();
    fireEvent.keyDown(carousel, { key: "ArrowLeft" });
    expect(screen.getByText("1 / 3")).toBeTruthy();
  });

  it("resets the active slide and image state when the review run changes", () => {
    const questions = {
      concept_image_path: "renders/concept.png",
      review_image_path: "renders/review.png",
    };
    const { rerender } = render(
      <ReviewWorkspaceFiles runId="run-one" questions={questions} />,
    );

    const firstImage = screen.getByRole("img", { name: "Concept image" });
    fireEvent.load(firstImage);
    fireEvent.click(screen.getByRole("button", { name: "Next review file" }));
    expect(screen.getByRole("img", { name: "Review image" })).toBeTruthy();

    rerender(<ReviewWorkspaceFiles runId="run-two" questions={questions} />);

    const resetImage = screen.getByRole("img", { name: "Concept image" });
    expect(resetImage.getAttribute("src")).toContain(
      "/api/runs/run-two/files/preview/renders/concept.png",
    );
    expect(
      screen.getByRole("status", { name: "Loading Concept image" }),
    ).toBeTruthy();
    expect(screen.getByText("1 / 2")).toBeTruthy();
  });

  it("loads and formats a JSON worktree file in a dialog", async () => {
    let resolveFile:
      | ((value: {
          path: string;
          content: string;
          binary: boolean;
          exists: boolean;
        }) => void)
      | undefined;
    apiMocks.getRunFileContent.mockReturnValue(
      new Promise((resolve) => {
        resolveFile = resolve;
      }),
    );

    render(
      <ReviewWorkspaceFiles
        runId="run-review"
        questions={{ plan_path: "plans/vertical-plan.json" }}
      />,
    );

    fireEvent.click(
      screen.getByRole("button", {
        name: "Open Plan (vertical-plan.json)",
      }),
    );
    expect(apiMocks.getRunFileContent).toHaveBeenCalledWith(
      "run-review",
      "plans/vertical-plan.json",
    );
    expect(
      screen.getByRole("status", { name: "Loading file preview" }),
    ).toBeTruthy();

    await act(async () => {
      resolveFile?.({
        path: "plans/vertical-plan.json",
        content: '{"approved":true,"epics":3}',
        binary: false,
        exists: true,
      });
      await Promise.resolve();
    });

    expect(screen.getByText(/"approved": true/)).toBeTruthy();
    expect(screen.getByText(/"epics": 3/)).toBeTruthy();
    expect(screen.getAllByText("vertical-plan.json")).toHaveLength(2);
    expect(screen.queryByText("plans/vertical-plan.json")).toBeNull();
  });

  it("shows a retryable error when a text file cannot be loaded", async () => {
    apiMocks.getRunFileContent.mockRejectedValueOnce(
      new Error("Preview service unavailable"),
    );

    render(
      <ReviewWorkspaceFiles
        runId="run-review"
        questions={{ reviewer_notes_path: "notes/review.txt" }}
      />,
    );
    fireEvent.click(
      screen.getByRole("button", {
        name: "Open Reviewer notes (review.txt)",
      }),
    );

    await waitFor(() => {
      expect(
        screen.getByText("This file could not be opened."),
      ).toBeTruthy();
    });
    expect(screen.getByText("Preview service unavailable")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Try again" }),
    ).toBeTruthy();
  });

  it("surfaces an image loading failure without exposing its full path", () => {
    const path = "renders/missing.webp";
    render(
      <ReviewWorkspaceFiles
        runId="run-review"
        questions={{ concept_image_path: path }}
      />,
    );

    const image = screen.getByRole("img", { name: "Concept image" });
    fireEvent.error(image);
    expect(image.getAttribute("src")).toContain(
      "/pipeline-board/workspace-images/renders/missing.webp",
    );
    fireEvent.error(image);
    expect(screen.getByRole("alert").textContent).toContain(
      "Preview unavailable",
    );
    expect(screen.queryByText(path)).toBeNull();
  });
});
