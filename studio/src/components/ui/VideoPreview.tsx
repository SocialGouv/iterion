// VideoPreview plays a user-supplied video with the caption track its
// media set carries, when it carries one (RGAA 4.3). Nothing in the
// studio can caption third-party media — an operator's or a bot's
// artifact — so with no track the player says so, visibly: a bare
// <video> silently promises subtitles it never shows. The notice pairs
// with the media-scope line in the accessibility notes
// (studio/docs/design-system.md § Accessibility).

// The four <video> players this replaced were four copies of the same
// preview block; the sibling-lookup that fills captionsHref lives with
// the callers that hold a file list (ProducedElements, the review
// scope panel) — the ones that only hold a URL render the notice.
export function VideoPreview({
  src,
  captionsHref,
  label,
  className,
  autoPlay = false,
  onError,
}: {
  src: string;
  /** URL of the sibling WebVTT track, when the media set carries one. */
  captionsHref?: string | null;
  /** Visible name of the media; also the <track> label when captions exist. */
  label?: string;
  className?: string;
  autoPlay?: boolean;
  onError?: () => void;
}) {
  return (
    <div className="min-w-0">
      <video
        controls
        autoPlay={autoPlay || undefined}
        src={src}
        className={className}
        onError={onError}
      >
        {captionsHref ? (
          <track
            kind="captions"
            src={captionsHref}
            label={label ? `Captions — ${label}` : "Captions"}
            default
          />
        ) : null}
        Your browser cannot play this video file.
      </video>
      {!captionsHref && (
        <p className="mt-1 text-micro text-fg-subtle">
          No caption track was provided for this video.
        </p>
      )}
    </div>
  );
}
