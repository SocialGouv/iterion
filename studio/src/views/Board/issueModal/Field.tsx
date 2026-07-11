import { FieldLabel } from "@/components/ui/FieldLabel";
import { RequiredPill } from "@/lib/varValidation";

// Field pairs the canonical ui/FieldLabel caption with the control
// below it (the house sibling pattern — see NewTriggerDialog).
export function Field({
  label,
  children,
  required,
  help,
}: {
  label: string;
  required?: boolean;
  // Optional help text, surfaced as FieldLabel's `?` affordance.
  help?: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <FieldLabel help={help}>
        {label}
        {required && (
          <>
            {" "}
            <RequiredPill />
          </>
        )}
      </FieldLabel>
      {children}
    </div>
  );
}
