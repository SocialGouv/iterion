import { BRAND_LOGO_CIRCLE_URL, BRAND_LOGO_URL } from "./forgeShared";

// GitHub can neither take an App's logo from the manifest nor set it through
// an API: giving a manifest-created App the iterion-bot face is the one upload
// left to the operator. This is the hand-off — the file, and the exact page.
export function GitHubAppLogoHint({
  logoUploadURL,
  appLabel,
}: {
  /** The App's General settings page (Display information), when iterion
   *  recorded it — a manifest-created App always has one. */
  logoUploadURL?: string;
  appLabel?: string;
}) {
  return (
    <div
      className="flex items-start gap-3 rounded border border-accent/40 bg-accent-soft p-3"
      data-testid="github-app-logo-hint"
    >
      <img
        src={BRAND_LOGO_URL}
        alt=""
        aria-hidden
        className="h-12 w-12 shrink-0 rounded-full bg-surface-2"
      />
      <div className="min-w-0 space-y-1.5">
        <div className="text-sm font-medium text-fg-default">
          Give {appLabel ?? "this App"} the iterion-bot face
        </div>
        <p className="text-caption text-fg-muted">
          GitHub cannot take a logo from the manifest and has no API for it, so this
          upload is yours: download the logo, then drop it under{" "}
          <span className="font-medium">Display information</span> on the App&apos;s
          settings page. Every comment and commit the App signs will carry it.
        </p>
        <div className="flex flex-wrap items-center gap-3 text-caption">
          <a
            href={BRAND_LOGO_URL}
            download="iterion-bot.png"
            className="text-accent-text hover:underline"
          >
            Download the logo
          </a>
          <a
            href={BRAND_LOGO_CIRCLE_URL}
            download="iterion-bot-circle.png"
            className="text-accent-text hover:underline"
          >
            Badge version
          </a>
          {logoUploadURL ? (
            <a
              href={logoUploadURL}
              target="_blank"
              rel="noreferrer"
              className="text-accent-text hover:underline"
            >
              Open the App&apos;s settings ↗
            </a>
          ) : (
            <span className="text-fg-subtle">
              GitHub → Settings → Developer settings → GitHub Apps → this App.
            </span>
          )}
        </div>
      </div>
    </div>
  );
}
