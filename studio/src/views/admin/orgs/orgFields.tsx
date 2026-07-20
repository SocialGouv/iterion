// Small presentational bits shared by the org-drawer sections.

export function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block text-xs space-y-1">
      <span className="text-fg-muted">{label}</span>
      <div>{children}</div>
    </label>
  );
}

export function Stat({
  title,
  value,
  progress,
}: {
  title: string;
  value: string;
  progress?: number | null;
}) {
  return (
    <div className="bg-surface-0 border border-border-subtle rounded p-2">
      <div className="text-fg-muted">{title}</div>
      <div className="font-medium">{value}</div>
      {progress != null && (
        <div
          className="mt-1 h-1 bg-surface-2 rounded overflow-hidden"
          role="progressbar"
          aria-valuenow={Math.round(progress)}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={`${title} usage`}
        >
          <div
            className={`h-full ${progress > 90 ? "bg-danger" : progress > 70 ? "bg-warning" : "bg-accent"}`}
            style={{ width: `${progress}%` }}
          />
        </div>
      )}
    </div>
  );
}
