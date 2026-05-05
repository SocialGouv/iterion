import { useEffect, useState } from "react";

import { desktop, type AppInfo } from "@/lib/desktopBridge";

export default function AboutTab() {
  const [info, setInfo] = useState<AppInfo | null>(null);

  useEffect(() => {
    desktop.getAppInfo().then(setInfo).catch(() => setInfo(null));
  }, []);

  if (!info) return <p className="text-fg-subtle p-4">Loading…</p>;
  return (
    <div className="flex flex-col gap-3 p-4 text-sm">
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
        <dt className="text-fg-subtle">Version</dt>
        <dd>{info.version}</dd>
        <dt className="text-fg-subtle">Commit</dt>
        <dd>{info.commit || "—"}</dd>
        <dt className="text-fg-subtle">Platform</dt>
        <dd>
          {info.os}/{info.arch}
        </dd>
        <dt className="text-fg-subtle">License</dt>
        <dd>{info.license}</dd>
      </dl>
      <ul className="flex flex-col gap-1 text-xs">
        {(
          [
            ["GitHub", info.homepage],
            ["Documentation", info.documentation],
            ["Report an issue", info.issue_tracker],
          ] as const
        ).map(([label, url]) => (
          <li key={label}>
            <button
              className="text-accent underline"
              onClick={() => desktop.openExternal(url)}
            >
              {label}
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}
