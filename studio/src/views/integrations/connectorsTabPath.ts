// connectorsTabPath deep-links the Connectors tab of Integrations: the tab,
// the connector its add form starts on, and the in-app path to go back to once
// the connection exists. ConnectionsTab reads `connector` and `returnTo` back,
// and admits the return target only through sanitizeReturnTo.
export function connectorsTabPath(opts: { connector?: string; returnTo?: string } = {}): string {
  const p = new URLSearchParams({ tab: "connectors" });
  if (opts.connector) p.set("connector", opts.connector);
  if (opts.returnTo) p.set("returnTo", opts.returnTo);
  return `/integrations?${p}`;
}
