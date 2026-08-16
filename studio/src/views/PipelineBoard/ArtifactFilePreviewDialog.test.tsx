import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

import type { PreviewState } from "@/components/Runs/usePreview";

vi.mock("./ImagePreview", () => ({
  ImagePreviewDialog: ({ alt }: { alt: string }) => <img alt={alt} />,
  MediaPreviewDialog: ({
    alt,
    kind,
  }: {
    alt: string;
    kind?: string;
  }) => (kind === "image" || !kind ? <img alt={alt} /> : <div data-kind={kind}>{alt}</div>),
}));

import {
  ArtifactFilePreviewDialog,
  ArtifactPreviewBody,
} from "./ArtifactFilePreviewDialog";

function loaded(path: string): PreviewState {
  return {
    path,
    size: 12,
    loading: false,
    error: null,
    textBody: null,
    blobURL: `blob:${path}`,
    contentType: "application/octet-stream",
  };
}

describe("ArtifactPreviewBody", () => {
  it("uses the validated media kind when a store serves octet-stream", () => {
    expect(
      renderToStaticMarkup(<ArtifactPreviewBody preview={loaded("cover.png")} kind="image" />),
    ).toContain("<img");
    expect(
      renderToStaticMarkup(<ArtifactPreviewBody preview={loaded("track.wav")} kind="audio" />),
    ).toContain("<audio");
    expect(
      renderToStaticMarkup(<ArtifactPreviewBody preview={loaded("clip.mp4")} kind="video" />),
    ).toContain("<video");
  });

  it("uses an explicit review description as the image alternative", () => {
    const html = renderToStaticMarkup(
      <ArtifactFilePreviewDialog
        preview={loaded("cover.png")}
        runId="run-1"
        kind="image"
        imageAlt="Final cover artwork"
        onClose={() => {}}
      />,
    );
    expect(html).toContain('alt="Final cover artwork"');
  });

  it("embeds a validated PDF inside Iterion", () => {
    const preview = {
      ...loaded("plan.pdf"),
      contentType: "application/pdf",
    };
    const html = renderToStaticMarkup(
      <ArtifactPreviewBody preview={preview} kind="doc" imageAlt="Release plan" />,
    );
    expect(html).toContain("<iframe");
    expect(html).toContain('title="Release plan"');
  });
});
