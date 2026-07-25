import {
  ChevronLeftIcon,
  ChevronRightIcon,
  FileTextIcon,
  ImageIcon,
} from "@radix-ui/react-icons";
import {
  type KeyboardEvent,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { getRunFileContent, runFilePreviewURL } from "@/api/runs";
import { pipelineBoardImageURL } from "@/api/pipelineBoards";
import { Button, Dialog, Spinner } from "@/components/ui";
import { humanizeKey } from "@/lib/humanizeKey";

import { ImagePreviewDialog } from "./ImagePreview";

const IMAGE_EXTENSIONS = new Set(["png", "jpg", "jpeg", "webp", "gif"]);
const TEXT_EXTENSIONS = new Set([
  "json",
  "md",
  "txt",
  "yaml",
  "yml",
  "csv",
]);
const GENERIC_LABEL_KEYS = new Set([
  "attachment",
  "attachments",
  "file",
  "files",
  "media",
  "media_ref",
  "media_refs",
  "path",
  "paths",
]);
const FILE_KEY_RE =
  /(?:^|_)(?:artifacts?|attachments?|csv|docs?|documents?|files?|images?|json|markdown|media|outputs?|paths?|plans?|previews?|refs?|references?|text|ya?ml)(?:_|$)/i;

export type ReviewWorkspaceFileKind = "image" | "text";

export interface ReviewWorkspaceFile {
  path: string;
  name: string;
  label: string;
  kind: ReviewWorkspaceFileKind;
  extension: string;
}

interface ReviewWorkspaceFilesProps {
  questions: Record<string, unknown> | null | undefined;
  runId: string;
  /**
   * Optional rendered human-node instructions. Backticked file references in
   * the prose enrich terse schema keys with their nearby human description and
   * provide a compatibility fallback for older pauses that only mentioned a
   * review file in their instructions.
   */
  instructions?: string;
}

interface TextPreviewState {
  reviewKey: string;
  file: ReviewWorkspaceFile;
  loading: boolean;
  content: string;
  error: string | null;
}

type ImageLoadState =
  | "loaded-run"
  | "loading-fallback"
  | "loaded-fallback"
  | "error";

/**
 * Extract previewable, workdir-relative file references from a human pause's
 * questions payload. The traversal accepts direct strings, arrays, nested
 * `{path, caption}` records, and JSON-encoded collections. URLs, absolute
 * paths, traversal segments, prose, unsupported extensions, and duplicates
 * are ignored.
 */
// The helper intentionally lives beside the autonomous component: consumers
// use the same extraction contract for presence checks and rendering.
// eslint-disable-next-line react-refresh/only-export-components
export function reviewWorkspaceFilesFromQuestions(
  questions: Record<string, unknown> | null | undefined,
  instructions?: string,
): ReviewWorkspaceFile[] {
  const files: ReviewWorkspaceFile[] = [];
  const seen = new Set<string>();

  const add = (
    rawPath: string,
    keyHint: string,
    explicitLabel?: string,
    pathIsExplicit = false,
  ) => {
    const file = workspaceFileFromCandidate(
      rawPath,
      keyHint,
      explicitLabel,
      pathIsExplicit,
    );
    if (!file || seen.has(file.path)) return;
    seen.add(file.path);
    files.push(file);
  };

  const visit = (
    value: unknown,
    keyHint: string,
    pathIsExplicit = false,
  ): void => {
    if (typeof value === "string") {
      const structured = parseStructuredValue(value);
      if (structured !== undefined) {
        visit(structured, keyHint, pathIsExplicit);
        return;
      }
      add(value, keyHint, undefined, pathIsExplicit);
      return;
    }

    if (Array.isArray(value)) {
      for (const entry of value) visit(entry, keyHint, pathIsExplicit);
      return;
    }

    if (!isRecord(value)) return;

    const explicitPath = typeof value.path === "string" ? value.path : null;
    if (explicitPath) {
      add(
        explicitPath,
        keyHint,
        firstNonEmptyString(
          value.caption,
          value.label,
          value.title,
          value.name,
        ),
        true,
      );
    }

    for (const [key, child] of Object.entries(value)) {
      if (
        key === "path" ||
        key === "caption" ||
        key === "label" ||
        key === "title" ||
        key === "name"
      ) {
        continue;
      }
      visit(child, key, pathIsExplicit || FILE_KEY_RE.test(key));
    }
  };

  for (const [key, value] of Object.entries(questions ?? {})) {
    visit(value, key, FILE_KEY_RE.test(key));
  }
  return mergeInstructionFiles(files, instructions);
}

/**
 * ReviewWorkspaceFiles keeps review references inside Iterion: images are
 * visible as thumbnails and open in the zoomable image viewer, while text/data
 * files are read lazily from the paused run's live workdir.
 */
export function ReviewWorkspaceFiles({
  questions,
  runId,
  instructions,
}: ReviewWorkspaceFilesProps) {
  const files = useMemo(
    () => reviewWorkspaceFilesFromQuestions(questions, instructions),
    [questions, instructions],
  );
  const filesKey = files.map((file) => file.path).join("\u0000");
  const reviewKey = `${runId}\u0000${filesKey}`;
  const [activeSelection, setActiveSelection] = useState<{
    reviewKey: string;
    path: string;
  } | null>(null);
  const [selectedImageState, setSelectedImageState] = useState<{
    reviewKey: string;
    file: ReviewWorkspaceFile;
  } | null>(null);
  const [textPreview, setTextPreview] = useState<TextPreviewState | null>(
    null,
  );
  const [imageStates, setImageStates] = useState<
    Record<string, ImageLoadState>
  >({});
  const requestGeneration = useRef(0);
  const currentReviewKey = useRef(reviewKey);
  const activePath =
    activeSelection?.reviewKey === reviewKey ? activeSelection.path : null;
  const selectedImage =
    selectedImageState?.reviewKey === reviewKey
      ? selectedImageState.file
      : null;
  const displayedTextPreview =
    textPreview?.reviewKey === reviewKey ? textPreview : null;

  useEffect(
    () => () => {
      requestGeneration.current += 1;
    },
    [],
  );

  useEffect(() => {
    currentReviewKey.current = reviewKey;
    requestGeneration.current += 1;
  }, [reviewKey]);

  const openTextPreview = useCallback(
    (file: ReviewWorkspaceFile) => {
      const generation = ++requestGeneration.current;
      const openedReviewKey = reviewKey;
      setTextPreview({
        reviewKey: openedReviewKey,
        file,
        loading: true,
        content: "",
        error: null,
      });
      void getRunFileContent(runId, file.path)
        .then((result) => {
          if (
            generation !== requestGeneration.current ||
            currentReviewKey.current !== openedReviewKey
          ) {
            return;
          }
          if (!result.exists) {
            setTextPreview({
              reviewKey: openedReviewKey,
              file,
              loading: false,
              content: "",
              error: "This file is no longer available.",
            });
            return;
          }
          if (result.binary) {
            setTextPreview({
              reviewKey: openedReviewKey,
              file,
              loading: false,
              content: "",
              error: "This file cannot be previewed as text.",
            });
            return;
          }
          setTextPreview({
            reviewKey: openedReviewKey,
            file,
            loading: false,
            content: formatTextContent(file, result.content),
            error: null,
          });
        })
        .catch((error: unknown) => {
          if (
            generation !== requestGeneration.current ||
            currentReviewKey.current !== openedReviewKey
          ) {
            return;
          }
          setTextPreview({
            reviewKey: openedReviewKey,
            file,
            loading: false,
            content: "",
            error:
              error instanceof Error
                ? error.message
                : "The file could not be loaded.",
          });
        });
    },
    [reviewKey, runId],
  );

  const closeTextPreview = useCallback(() => {
    requestGeneration.current += 1;
    setTextPreview(null);
  }, []);

  const setImageState = (path: string, state: ImageLoadState) => {
    const stateKey = `${reviewKey}\u0000${path}`;
    setImageStates((current) =>
      current[stateKey] === state
        ? current
        : { ...current, [stateKey]: state },
    );
  };

  if (files.length === 0) return null;

  const selectedIndex = activePath
    ? files.findIndex((file) => file.path === activePath)
    : 0;
  const activeIndex = selectedIndex >= 0 ? selectedIndex : 0;
  const activeFile = files[activeIndex];
  if (!activeFile) return null;

  const showFile = (index: number) => {
    const next = files[Math.min(Math.max(index, 0), files.length - 1)];
    if (next) setActiveSelection({ reviewKey, path: next.path });
  };

  const handleCarouselKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    let nextIndex: number;
    switch (event.key) {
      case "ArrowLeft":
        nextIndex = activeIndex - 1;
        break;
      case "ArrowRight":
        nextIndex = activeIndex + 1;
        break;
      case "Home":
        nextIndex = 0;
        break;
      case "End":
        nextIndex = files.length - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    event.stopPropagation();
    showFile(nextIndex);
  };

  const activeImageState =
    imageStates[`${reviewKey}\u0000${activeFile.path}`];
  const activeUsesWorkspaceFallback =
    activeImageState === "loading-fallback" ||
    activeImageState === "loaded-fallback";
  const activeImageSource = activeUsesWorkspaceFallback
    ? pipelineBoardImageURL(activeFile.path)
    : runFilePreviewURL(runId, activeFile.path);
  const activeImageFailed = activeImageState === "error";
  const activeImageLoaded =
    activeImageState === "loaded-run" ||
    activeImageState === "loaded-fallback";

  return (
    <section
      aria-label="Files to review"
      className="space-y-3 rounded-lg border border-border-default bg-surface-0 p-3"
    >
      <header className="space-y-0.5">
        <h3 className="text-sm font-semibold text-fg-default">
          Review these files
        </h3>
        <p className="text-xs leading-relaxed text-fg-muted">
          Open the visual and supporting files here before answering.
        </p>
      </header>

      <div
        role="group"
        aria-label="Review files carousel"
        aria-roledescription="carousel"
        aria-keyshortcuts="ArrowLeft ArrowRight Home End"
        tabIndex={0}
        onKeyDown={handleCarouselKeyDown}
        className="space-y-3 rounded-lg outline-none focus-visible:ring-2 focus-visible:ring-accent"
      >
        <div
          role="group"
          aria-roledescription="slide"
          aria-label={`File ${activeIndex + 1} of ${files.length}: ${activeFile.label}`}
        >
          {activeFile.kind === "image" ? (
            <button
              type="button"
              onClick={() =>
                setSelectedImageState({ reviewKey, file: activeFile })
              }
              disabled={activeImageFailed}
              aria-label={`Open ${activeFile.label} (${activeFile.name})`}
              className="group w-full overflow-hidden rounded-lg border border-border-subtle bg-surface-1 text-left transition-colors hover:border-border-strong hover:bg-surface-2 disabled:cursor-not-allowed"
            >
              <span className="relative flex aspect-[16/10] max-h-80 w-full items-center justify-center overflow-hidden bg-surface-2/60">
                {!activeImageFailed && !activeImageLoaded && (
                  <Spinner
                    size="md"
                    label={`Loading ${activeFile.label}`}
                    className="absolute text-fg-subtle"
                  />
                )}
                {activeImageFailed ? (
                  <span
                    className="flex flex-col items-center gap-2 px-3 text-center text-sm text-danger-fg"
                    role="alert"
                  >
                    <ImageIcon className="h-8 w-8" aria-hidden />
                    Preview unavailable
                  </span>
                ) : (
                  <img
                    src={activeImageSource}
                    alt={activeFile.label}
                    loading="lazy"
                    onLoad={() =>
                      setImageState(
                        activeFile.path,
                        activeUsesWorkspaceFallback
                          ? "loaded-fallback"
                          : "loaded-run",
                      )
                    }
                    onError={() =>
                      setImageState(
                        activeFile.path,
                        activeUsesWorkspaceFallback
                          ? "error"
                          : "loading-fallback",
                      )
                    }
                    className={`h-full w-full object-contain transition-opacity ${
                      activeImageLoaded ? "opacity-100" : "opacity-0"
                    }`}
                  />
                )}
              </span>
              <FileCaption file={activeFile} action="Open full-size image" />
            </button>
          ) : (
            <button
              type="button"
              onClick={() => openTextPreview(activeFile)}
              aria-label={`Open ${activeFile.label} (${activeFile.name})`}
              className="flex min-h-64 w-full flex-col items-center justify-center gap-4 rounded-lg border border-border-subtle bg-surface-1 px-6 py-10 text-center transition-colors hover:border-border-strong hover:bg-surface-2"
            >
              <FileTextIcon
                className="h-14 w-14 text-accent-text"
                aria-hidden
              />
              <span className="space-y-1">
                <span className="block text-lg font-semibold text-fg-default">
                  {activeFile.label}
                </span>
                <span
                  className="block break-all text-sm text-fg-muted"
                  title={activeFile.path}
                >
                  {activeFile.name}
                </span>
              </span>
              <span className="text-sm font-medium text-accent-text">
                Open preview
              </span>
            </button>
          )}
        </div>

        {files.length > 1 ? (
          <div className="flex items-center justify-between gap-3">
            <Button
              variant="secondary"
              size="sm"
              onClick={() => showFile(activeIndex - 1)}
              disabled={activeIndex === 0}
              aria-label="Previous review file"
            >
              <ChevronLeftIcon aria-hidden />
              Previous
            </Button>
            <span
              className="text-sm font-medium tabular-nums text-fg-muted"
              aria-live="polite"
              aria-atomic="true"
            >
              {activeIndex + 1} / {files.length}
            </span>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => showFile(activeIndex + 1)}
              disabled={activeIndex === files.length - 1}
              aria-label="Next review file"
            >
              Next
              <ChevronRightIcon aria-hidden />
            </Button>
          </div>
        ) : (
          <span
            className="block text-center text-sm font-medium tabular-nums text-fg-muted"
            aria-label="1 review file"
          >
            1 / 1
          </span>
        )}

        {files.length > 1 && files.length <= 10 && (
          <div
            role="group"
            aria-label="Choose a review file"
            className="flex flex-wrap justify-center gap-2"
          >
            {files.map((file, index) => (
              <button
                key={file.path}
                type="button"
                onClick={() => showFile(index)}
                aria-label={`Show file ${index + 1}: ${file.label}`}
                aria-current={index === activeIndex ? "true" : undefined}
                className={`h-2.5 w-2.5 rounded-full border transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent ${
                  index === activeIndex
                    ? "border-accent bg-accent"
                    : "border-border-strong bg-surface-2 hover:bg-surface-3"
                }`}
              />
            ))}
          </div>
        )}
      </div>

      {selectedImage && (
        <ImagePreviewDialog
          open
          onOpenChange={(open) => {
            if (!open) setSelectedImageState(null);
          }}
          src={
            imageStates[`${reviewKey}\u0000${selectedImage.path}`] ===
            "loaded-fallback"
              ? pipelineBoardImageURL(selectedImage.path)
              : runFilePreviewURL(runId, selectedImage.path)
          }
          alt={selectedImage.label}
          title={selectedImage.label}
          description={selectedImage.name}
          downloadHref={
            imageStates[`${reviewKey}\u0000${selectedImage.path}`] ===
            "loaded-fallback"
              ? pipelineBoardImageURL(selectedImage.path)
              : runFilePreviewURL(runId, selectedImage.path)
          }
        />
      )}

      {displayedTextPreview && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) closeTextPreview();
          }}
          widthClass="max-w-4xl"
          title={displayedTextPreview.file.label}
          description={displayedTextPreview.file.name}
        >
          {displayedTextPreview.loading ? (
            <div className="flex h-48 items-center justify-center gap-2 text-sm text-fg-muted">
              <Spinner size="md" label="Loading file preview" />
              Loading preview…
            </div>
          ) : displayedTextPreview.error ? (
            <div
              className="space-y-3 rounded-lg border border-danger/40 bg-danger-soft p-3"
              role="alert"
            >
              <p className="text-sm font-medium text-danger-fg">
                This file could not be opened.
              </p>
              <p className="break-words text-xs text-danger-fg">
                {displayedTextPreview.error}
              </p>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => openTextPreview(displayedTextPreview.file)}
              >
                Try again
              </Button>
            </div>
          ) : displayedTextPreview.content ? (
            <pre className="max-h-[70vh] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-surface-0 p-4 font-mono text-sm leading-relaxed text-fg-default">
              {displayedTextPreview.content}
            </pre>
          ) : (
            <p className="py-10 text-center text-sm text-fg-muted">
              This file is empty.
            </p>
          )}
        </Dialog>
      )}
    </section>
  );
}

function FileCaption({
  file,
  action,
}: {
  file: ReviewWorkspaceFile;
  action: string;
}) {
  return (
    <span className="flex items-center gap-3 px-3 py-3">
      <span className="min-w-0 flex-1 space-y-0.5">
        <span className="block text-sm font-medium text-fg-default">
          {file.label}
        </span>
        <span
          className="block truncate text-xs text-fg-muted"
          title={file.path}
        >
          {file.name}
        </span>
      </span>
      <span className="shrink-0 text-xs font-medium text-accent-text">
        {action}
      </span>
    </span>
  );
}

function mergeInstructionFiles(
  fromQuestions: ReviewWorkspaceFile[],
  instructions: string | undefined,
): ReviewWorkspaceFile[] {
  const fromInstructions = reviewWorkspaceFilesFromInstructions(instructions);
  if (fromInstructions.length === 0) return fromQuestions;

  const questionByPath = new Map(
    fromQuestions.map((file) => [file.path, file]),
  );
  const merged = fromInstructions.map((described) => {
    const detected = questionByPath.get(described.path);
    if (!detected) return described;
    questionByPath.delete(described.path);
    return { ...detected, label: described.label };
  });
  for (const file of fromQuestions) {
    if (questionByPath.has(file.path)) merged.push(file);
  }
  return merged;
}

function reviewWorkspaceFilesFromInstructions(
  instructions: string | undefined,
): ReviewWorkspaceFile[] {
  if (!instructions) return [];
  const files: ReviewWorkspaceFile[] = [];
  const seen = new Set<string>();
  for (const line of instructions.split(/\r?\n/)) {
    const matches = line.matchAll(/`([^`\r\n]+)`/g);
    for (const match of matches) {
      const rawPath = match[1];
      if (!rawPath) continue;
      const after = line.slice((match.index ?? 0) + match[0].length);
      const descriptionMatch = after.match(
        /^\s*(?::|—|-)\s*([^;\r\n]+?)(?:[.;]\s*)?$/,
      );
      const file = workspaceFileFromCandidate(
        rawPath,
        "review_file",
        descriptionMatch?.[1],
        true,
      );
      if (!file || seen.has(file.path)) continue;
      seen.add(file.path);
      files.push(file);
    }
  }
  return files;
}

function workspaceFileFromCandidate(
  rawPath: string,
  keyHint: string,
  explicitLabel?: string,
  pathIsExplicit = false,
): ReviewWorkspaceFile | null {
  const path = safeRelativePath(rawPath, pathIsExplicit || FILE_KEY_RE.test(keyHint));
  if (!path) return null;
  const name = path.slice(path.lastIndexOf("/") + 1);
  const dot = name.lastIndexOf(".");
  if (dot <= 0 || dot === name.length - 1) return null;
  const extension = name.slice(dot + 1).toLowerCase();
  const kind: ReviewWorkspaceFileKind | null = IMAGE_EXTENSIONS.has(extension)
    ? "image"
    : TEXT_EXTENSIONS.has(extension)
      ? "text"
      : null;
  if (!kind) return null;
  return {
    path,
    name,
    label: friendlyFileLabel(keyHint, name, explicitLabel),
    kind,
    extension,
  };
}

function safeRelativePath(raw: string, allowWhitespace: boolean): string | null {
  let path = raw.trim();
  while (path.startsWith("./")) path = path.slice(2);
  if (
    !path ||
    path.length > 2048 ||
    path.startsWith("/") ||
    path.includes("\\") ||
    path.includes("\0") ||
    path.includes("\n") ||
    path.includes("\r") ||
    path.includes("?") ||
    path.includes("#") ||
    (!allowWhitespace && /\s/.test(path)) ||
    /^[a-z][a-z0-9+.-]*:/i.test(path)
  ) {
    return null;
  }
  const segments = path.split("/");
  if (
    segments.some(
      (segment) => !segment || segment === "." || segment === "..",
    )
  ) {
    return null;
  }
  return path;
}

function friendlyFileLabel(
  keyHint: string,
  name: string,
  explicitLabel?: string,
): string {
  const described = conciseHumanLabel(explicitLabel);
  if (described) return described;

  const keyParts = keyHint.split(".");
  const leafKey = keyParts[keyParts.length - 1] ?? keyHint;
  const withoutSuffix = leafKey
    .replace(/(?:_paths?|_files?)$/i, "")
    .replace(/(?:Paths?|Files?)$/, "");
  const normalized = withoutSuffix.toLowerCase();
  if (withoutSuffix && !GENERIC_LABEL_KEYS.has(normalized)) {
    return humanizeKey(withoutSuffix);
  }

  const stem = name.replace(/\.[^.]+$/, "").replace(/[-_]+/g, " ");
  return stem ? humanizeKey(stem) : "File to review";
}

function conciseHumanLabel(value: string | undefined): string | null {
  const clean = value?.trim().replace(/[.;]\s*$/, "");
  if (!clean) return null;
  const truncated =
    clean.length > 120 ? `${clean.slice(0, 117).trimEnd()}…` : clean;
  return truncated.charAt(0).toUpperCase() + truncated.slice(1);
}

function parseStructuredValue(value: string): unknown | undefined {
  const trimmed = value.trim();
  if (
    trimmed.length < 2 ||
    !(
      (trimmed.startsWith("[") && trimmed.endsWith("]")) ||
      (trimmed.startsWith("{") && trimmed.endsWith("}"))
    )
  ) {
    return undefined;
  }
  try {
    return JSON.parse(trimmed);
  } catch {
    return undefined;
  }
}

function formatTextContent(
  file: ReviewWorkspaceFile,
  content: string,
): string {
  if (file.extension !== "json" || !content.trim()) return content;
  try {
    return JSON.stringify(JSON.parse(content), null, 2);
  } catch {
    return content;
  }
}

function firstNonEmptyString(...values: unknown[]): string | undefined {
  return values.find(
    (value): value is string =>
      typeof value === "string" && value.trim().length > 0,
  );
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

export default ReviewWorkspaceFiles;
