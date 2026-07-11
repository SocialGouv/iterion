import { RequiredPill } from "@/lib/varValidation";

export function Field({
  label,
  children,
  required,
}: {
  label: string;
  required?: boolean;
  children: React.ReactNode;
}) {
  return (
    <label className="block">
      <span className="text-xs text-fg-muted mb-1 flex items-baseline gap-2">
        {label}
        {required && <RequiredPill />}
      </span>
      {children}
    </label>
  );
}
