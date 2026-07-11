import { FieldLabel } from "@/components/ui/FieldLabel";
import { RequiredPill } from "@/lib/varValidation";

// Field pairs the canonical ui/FieldLabel caption with the control
// below it (the house sibling pattern — see NewTriggerDialog).
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
    <div>
      <FieldLabel>
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
