import { describe, expect, it } from "vitest";

import { classifyProducedFile, producedKindLabel } from "./fileKind";

describe("classifyProducedFile", () => {
  it("classifies by extension across media kinds", () => {
    expect(classifyProducedFile("pkg/dsl/ir/compile.go")).toBe("code");
    expect(classifyProducedFile("src/App.tsx")).toBe("code");
    expect(classifyProducedFile("assets/cover.png")).toBe("image");
    expect(classifyProducedFile("out/theme.mp3")).toBe("audio");
    expect(classifyProducedFile("renders/final.mp4")).toBe("video");
    expect(classifyProducedFile("docs/report.md")).toBe("doc");
    expect(classifyProducedFile("report.pdf")).toBe("doc");
    expect(classifyProducedFile("data/results.json")).toBe("data");
    expect(classifyProducedFile("data/metrics.csv")).toBe("data");
    expect(classifyProducedFile("dist/bundle.zip")).toBe("archive");
  });

  it("is case-insensitive on the extension", () => {
    expect(classifyProducedFile("COVER.PNG")).toBe("image");
    expect(classifyProducedFile("Theme.MP3")).toBe("audio");
  });

  it("treats extensionless files and leading-dot dotfiles as other", () => {
    expect(classifyProducedFile("Makefile")).toBe("other");
    expect(classifyProducedFile("LICENSE")).toBe("other");
    expect(classifyProducedFile(".gitignore")).toBe("other");
    // Only the final segment counts: ".env.local" resolves to ext "local".
    expect(classifyProducedFile("path/to/.env.local")).toBe("other");
  });

  it("uses the basename, ignoring dots in parent directories", () => {
    expect(classifyProducedFile("my.dir/output")).toBe("other");
    expect(classifyProducedFile("v1.2/song.wav")).toBe("audio");
  });
});

describe("producedKindLabel", () => {
  it("names each kind", () => {
    expect(producedKindLabel("code")).toBe("Code");
    expect(producedKindLabel("image")).toBe("Image");
    expect(producedKindLabel("audio")).toBe("Audio");
    expect(producedKindLabel("video")).toBe("Video");
    expect(producedKindLabel("other")).toBe("File");
  });
});
