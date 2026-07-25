import { useEffect, useState } from "react";

import { desktop, type AppInfo } from "@/lib/desktopBridge";
import { useServerInfoStore } from "@/store/serverInfo";
import { InlineBanner } from "@/components/ui/InlineBanner";
import PanelLoading from "@/components/shared/PanelLoading";

interface Props {
  desktopFeatures: boolean;
}

export default function AboutTab({ desktopFeatures }: Props) {
  const [info, setInfo] = useState<AppInfo | null>(null);
  const [error, setError] = useState<string | null>(null);
  const serverInfo = useServerInfoStore((s) => s.info);

  useEffect(() => {
    if (!desktopFeatures) return;
    desktop
      .getAppInfo()
      .then((i) => {
        setInfo(i);
        setError(null);
      })
      .catch((e) => {
        setInfo(null);
        setError((e as Error)?.message ?? "Failed to load app info.");
      });
  }, [desktopFeatures]);

  if (desktopFeatures && !info) {
    if (error) {
      return (
        <div className="p-4">
          <InlineBanner
            tone="danger"
            layout="inline"
            title="Failed to load app info"
          >
            {error}
          </InlineBanner>
        </div>
      );
    }
    return (
      <PanelLoading />
    );
  }

  return (
    <div className="flex flex-col gap-3 p-4 text-sm">
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
        <dt className="text-fg-subtle">Version</dt>
        <dd>{info?.version ?? serverInfo?.version ?? "—"}</dd>
        <dt className="text-fg-subtle">Commit</dt>
        <dd>{info?.commit || serverInfo?.commit || "—"}</dd>
        {info && (
          <>
            <dt className="text-fg-subtle">Platform</dt>
            <dd>
              {info.os}/{info.arch}
            </dd>
            <dt className="text-fg-subtle">License</dt>
            <dd>{info.license}</dd>
          </>
        )}
        {!info && serverInfo && (
          <>
            <dt className="text-fg-subtle">Mode</dt>
            <dd>{serverInfo.mode}</dd>
          </>
        )}
      </dl>
      <ul className="flex flex-col gap-1 text-xs">
        {(
          [
            ["GitHub", info?.homepage ?? "https://github.com/SocialGouv/iterion"],
            [
              "Documentation",
              info?.documentation ?? "https://socialgouv.github.io/iterion/",
            ],
            [
              "Report an issue",
              info?.issue_tracker ??
                "https://github.com/SocialGouv/iterion/issues",
            ],
          ] as const
        ).map(([label, url]) => (
          <li key={label}>
            {info ? (
              <button
                className="text-accent-text underline"
                onClick={() => desktop.openExternal(url)}
              >
                {label}
              </button>
            ) : (
              <a
                className="text-accent-text underline"
                href={url}
                target="_blank"
                rel="noreferrer"
              >
                {label}
              </a>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}
