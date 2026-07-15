import { errorMessage } from "@/lib/errorHints";
import { useState } from "react";

import { Button } from "@/components/ui";

import { desktop, type Release } from "@/lib/desktopBridge";

export default function UpdatesTab() {
  const [checking, setChecking] = useState(false);
  const [applying, setApplying] = useState(false);
  const [release, setRelease] = useState<Release | null | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);

  const handleCheck = async () => {
    setError(null);
    setChecking(true);
    try {
      setRelease(await desktop.checkForUpdate());
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setChecking(false);
    }
  };

  const handleApply = async () => {
    setError(null);
    setApplying(true);
    try {
      await desktop.downloadAndApplyUpdate();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setApplying(false);
    }
  };

  return (
    <div className="flex flex-col gap-3 p-4">
      <p className="text-xs text-fg-subtle">
        Updates are signed with Ed25519. Only manifests signed by the
        matching private key are accepted by the embedded verifier.
      </p>
      <div>
        <Button onClick={handleCheck} loading={checking} disabled={applying} variant="primary">
          Check for updates
        </Button>
      </div>
      {release === null && <p className="text-sm">You're up to date.</p>}
      {release && (
        <div className="flex flex-col gap-2 text-sm">
          <p>
            Update available: <strong>{release.version}</strong>
          </p>
          <button
            className="text-xs text-accent-text underline self-start"
            onClick={() => {
              // Defence-in-depth: refuse anything that isn't a plain
              // https:// URL before handing off to the OS shell. The
              // update manifest is signed but a future channel change
              // could let a release_notes_url with a custom protocol
              // (file:, javascript:, ms-...) ride along.
              const url = release.release_notes_url ?? "";
              if (url.startsWith("https://")) desktop.openExternal(url);
            }}
          >
            View release notes
          </button>
          <div>
            <Button onClick={handleApply} variant="primary" loading={applying}>
              Download &amp; apply
            </Button>
          </div>
        </div>
      )}
      {error && (
        <p className="text-danger text-sm" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
