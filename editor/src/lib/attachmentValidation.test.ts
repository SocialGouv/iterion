import { describe, expect, it } from "vitest";
import {
  formatBytes,
  mimeMatches,
  validateAttachment,
  totalSize,
} from "./attachmentValidation";
import type { AttachmentField, UploadLimits } from "../api/types";

const limits = (overrides: Partial<UploadLimits> = {}): UploadLimits => ({
  max_file_size: 50 * 1024 * 1024,
  max_total_size: 250 * 1024 * 1024,
  max_files_per_run: 10,
  allowed_mime: ["image/*", "application/pdf", "text/*"],
  ...overrides,
});

const file = (name: string, size: number, type: string): File => {
  const blob = new Blob([new Uint8Array(size)], { type });
  return new File([blob], name, { type });
};

const imageField: AttachmentField = { name: "logo", type: "image" };
const fileField: AttachmentField = { name: "spec", type: "file" };

describe("mimeMatches", () => {
  it("matches exact MIME", () => {
    expect(mimeMatches("image/png", "image/png")).toBe(true);
  });
  it("supports wildcard subtypes", () => {
    expect(mimeMatches("image/png", "image/*")).toBe(true);
    expect(mimeMatches("image/png", "text/*")).toBe(false);
  });
  it("tolerates content-type parameters", () => {
    expect(mimeMatches("text/plain; charset=utf-8", "text/plain")).toBe(true);
  });
  it("is case-insensitive", () => {
    expect(mimeMatches("IMAGE/PNG", "image/png")).toBe(true);
  });
  it("rejects empty inputs", () => {
    expect(mimeMatches("", "image/*")).toBe(false);
    expect(mimeMatches("image/png", "")).toBe(false);
  });
});

describe("formatBytes", () => {
  it("formats small sizes in bytes", () => {
    expect(formatBytes(512)).toBe("512 B");
  });
  it("formats KB/MB", () => {
    expect(formatBytes(2048)).toBe("2.0 KB");
    expect(formatBytes(50 * 1024 * 1024)).toBe("50 MB");
  });
});

describe("validateAttachment", () => {
  it("accepts a valid PNG against image field", () => {
    const r = validateAttachment(file("logo.png", 1024, "image/png"), imageField, limits());
    expect(r.ok).toBe(true);
  });

  it("rejects empty files", () => {
    const r = validateAttachment(file("blank.png", 0, "image/png"), imageField, limits());
    expect(r.ok).toBe(false);
    expect(r.error).toMatch(/empty/i);
  });

  it("rejects oversized files", () => {
    const r = validateAttachment(
      file("big.png", 100 * 1024 * 1024, "image/png"),
      imageField,
      limits({ max_file_size: 50 * 1024 * 1024 }),
    );
    expect(r.ok).toBe(false);
    expect(r.error).toMatch(/too large/i);
  });

  it("rejects MIME outside the field's allowlist", () => {
    const r = validateAttachment(file("bad.exe", 100, "application/x-msdownload"), imageField, limits());
    expect(r.ok).toBe(false);
    expect(r.error).toMatch(/not allowed/i);
  });

  it("accepts when server allowlist is more permissive than field", () => {
    const f: AttachmentField = { name: "doc", type: "file", accept_mime: ["application/pdf"] };
    const r = validateAttachment(file("spec.pdf", 100, "application/pdf"), f, limits());
    expect(r.ok).toBe(true);
  });

  it("respects the server's allowlist intersection", () => {
    const f: AttachmentField = { name: "doc", type: "file", accept_mime: ["video/mp4"] };
    const r = validateAttachment(file("clip.mp4", 100, "video/mp4"), f, limits());
    // Server allowlist excludes video/* by default.
    expect(r.ok).toBe(false);
  });
});

describe("totalSize", () => {
  it("sums sizes of selected attachments", () => {
    const a = { file: file("a.bin", 100, "text/plain") };
    const b = { file: file("b.bin", 200, "text/plain") };
    expect(totalSize({ a, b })).toBe(300);
  });
  it("ignores undefined entries", () => {
    expect(totalSize({ a: undefined })).toBe(0);
  });
});
