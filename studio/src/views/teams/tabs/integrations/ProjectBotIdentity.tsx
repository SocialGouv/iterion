// The "Will post as" answer for a connection that posts as a per-repository
// bot iterion mints at enable: the bot's login once it exists, and what will
// exist before the first enable — never an empty handle, never the person
// whose OAuth grant bootstrapped the connection.
export function ProjectBotIdentity({ handle }: { handle: string }) {
  if (handle) {
    return (
      <span className="text-fg-default">
        @{handle}{" "}
        <span className="text-fg-muted">(this repository's bot, created by iterion)</span>
      </span>
    );
  }
  return (
    <span className="text-fg-default">
      a dedicated project bot (iterion-bot) that iterion creates on this repository when you enable
      it
    </span>
  );
}
