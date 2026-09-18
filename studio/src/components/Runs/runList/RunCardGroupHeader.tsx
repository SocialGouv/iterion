// RunCardGroupHeader is the mobile-list group header — rendered above
// each group's cards via GroupedVirtuoso's groupContent. Visually subdued
// so the rows still dominate the scroll.
export function RunCardGroupHeader({
  label,
  count,
}: {
  label: string;
  count: number;
}) {
  return (
    <div className="px-4 py-1.5 bg-surface-2 border-y border-border-default text-fg-muted text-micro uppercase tracking-wide">
      <span>{label}</span>
      <span className="ml-2 text-fg-subtle normal-case tracking-normal">
        {count}
      </span>
    </div>
  );
}
