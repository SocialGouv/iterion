// A status region assistive tech actually hears (RGAA 7.5): the container
// is a permanent role="status" and the TEXT changes — toggling the
// visibility of a static string announces nothing, because nothing was
// injected. Kind is carried by the same text, never by motion alone.
export function StatusMessage({
  message,
  className = "",
}: {
  message: string;
  className?: string;
}) {
  return (
    <span role="status" className={className}>
      {message}
    </span>
  );
}
